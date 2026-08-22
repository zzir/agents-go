package server

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

const (
	// wsOutBuffer bounds a connection's outbound queue. It must hold a full
	// replay burst (RunHub.EventBufferCap == 512) plus live events arriving
	// during replay; overflow means a genuinely stuck client and closes it.
	wsOutBuffer = 1024
	// wsWriteTimeout caps a single socket write so a client whose TCP receive
	// window is full can never block the writer goroutine indefinitely.
	wsWriteTimeout = 15 * time.Second
	// wsMaxMessageBytes bounds a single inbound WebSocket frame. gorilla's
	// default read limit is unlimited, so without this cap a client — even an
	// unauthenticated one, before the auth handshake — could stream an
	// arbitrarily large frame straight into memory and OOM the process. 1 MiB
	// comfortably fits the largest legitimate inbound message (a chat prompt in
	// run.create, or the terminal.open handshake); a peer that exceeds it is
	// closed with 1009 (message too big).
	wsMaxMessageBytes = 1 << 20
	// wsAuthDeadline caps how long an upgraded-but-unauthenticated connection may
	// take to send its auth frame. Without it, a client that completes the
	// upgrade but never authenticates pins a goroutine and its read buffer
	// indefinitely. On success the heartbeat's rolling deadline takes over.
	wsAuthDeadline = 10 * time.Second
	// wsHandshakeTimeout bounds the upgrade handshake itself, so a slow client
	// dribbling the upgrade request cannot tie up the accepting goroutine.
	wsHandshakeTimeout = 10 * time.Second
	// wsPongWait is the heartbeat's read deadline: a connection that answers no
	// ping within it is half-open (NAT idled out, client gone without a close
	// frame) and is dropped instead of pinning its goroutine and outbound queue
	// until TCP keepalive notices, hours later.
	wsPongWait = 60 * time.Second
	// wsPingInterval is how often the heartbeat pings; well under wsPongWait so
	// one lost ping doesn't kill a healthy connection.
	wsPingInterval = 25 * time.Second
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: wsHandshakeTimeout,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
}

// WSConn is a websocket connection with a per-connection context and a write
// mutex for concurrent sends. Event delivery goes through a bounded outbound
// queue drained by a single writer goroutine (StartWriter), so a slow or stuck
// client can never back-pressure the producers (the run goroutine / hub) that
// enqueue events — it just fills its own queue and gets disconnected.
type WSConn struct {
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	out     chan any
	outOnce sync.Once

	// User is who authenticated the connection (HandleWSWithAuth); the fan-out
	// attaches a connection only to runs of sessions this user owns.
	User protocol.UserInfo
	// recheck resolves the credential the connection authenticated with,
	// again — Recheck's half.
	recheck func(context.Context) (protocol.UserInfo, error)

	// The heartbeat's read deadline, and whether PauseHeartbeat lifted it.
	hbMu     sync.Mutex
	hbPaused bool
	pongWait time.Duration
}

// PauseHeartbeat lifts the heartbeat's read deadline for a stretch in which
// the handler will not read — a terminal dialing a host, pulling an image —
// since pongs are only processed by a read, and a deadline nobody can
// extend would end the connection. ResumeHeartbeat re-arms it.
func (c *WSConn) PauseHeartbeat() {
	c.hbMu.Lock()
	c.hbPaused = true
	c.hbMu.Unlock()
	_ = c.conn.SetReadDeadline(time.Time{})
}

// ResumeHeartbeat re-arms the heartbeat's read deadline after PauseHeartbeat.
func (c *WSConn) ResumeHeartbeat() {
	c.hbMu.Lock()
	c.hbPaused = false
	c.hbMu.Unlock()
	_ = c.conn.SetReadDeadline(time.Now().Add(c.pongWait))
}

// Recheck resolves the connection's credential again and reports whether it
// still names the same user with the same role. When it does not — revoked,
// expired, demoted — the connection is closed with a policy-violation frame
// and false is returned: the client reconnects, and the reconnect's auth
// frame decides afresh. Called before a frame acts, so a revocation takes
// effect at the next action rather than at the next reconnect.
func (c *WSConn) Recheck() bool {
	if c.recheck == nil {
		return true
	}
	u, err := c.recheck(c.ctx)
	if err == nil && u.ID == c.User.ID && u.Role == c.User.Role {
		return true
	}
	c.CloseWith(websocket.ClosePolicyViolation, "credential no longer valid")
	return false
}

// CloseWith sends a close frame carrying code and reason, then closes.
func (c *WSConn) CloseWith(code int, reason string) {
	c.mu.Lock()
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(wsWriteTimeout))
	c.mu.Unlock()
	c.Close()
}

// ConnTracker holds every authenticated WebSocket connection by user, so a
// revocation or a role change can close what the old credential opened.
type ConnTracker struct {
	mu     sync.Mutex
	byUser map[string]map[*WSConn]struct{}
}

// NewConnTracker returns an empty tracker.
func NewConnTracker() *ConnTracker {
	return &ConnTracker{byUser: make(map[string]map[*WSConn]struct{})}
}

func (t *ConnTracker) add(c *WSConn) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	set := t.byUser[c.User.ID]
	if set == nil {
		set = make(map[*WSConn]struct{})
		t.byUser[c.User.ID] = set
	}
	set[c] = struct{}{}
}

func (t *ConnTracker) remove(c *WSConn) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if set := t.byUser[c.User.ID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(t.byUser, c.User.ID)
		}
	}
}

// CloseForUser closes every connection the user holds, with reason. Each
// client reconnects and authenticates afresh — a still-valid credential
// comes straight back, a revoked one is refused.
func (t *ConnTracker) CloseForUser(userID, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	conns := make([]*WSConn, 0, len(t.byUser[userID]))
	for c := range t.byUser[userID] {
		conns = append(conns, c)
	}
	t.mu.Unlock()
	for _, c := range conns {
		c.CloseWith(websocket.ClosePolicyViolation, reason)
	}
}

// Context returns the per-connection context, cancelled when the connection closes.
func (c *WSConn) Context() context.Context { return c.ctx }

// ReadJSON reads the next JSON message from the connection into v.
func (c *WSConn) ReadJSON(v any) error {
	return c.conn.ReadJSON(v)
}

// StartWriter launches the outbound writer goroutine (idempotent). After this,
// use WriteAsync for event delivery; the goroutine serializes writes and
// applies a deadline, so enqueueing never blocks on the network.
func (c *WSConn) StartWriter() {
	c.outOnce.Do(func() {
		c.out = make(chan any, wsOutBuffer)
		go c.writeLoop()
	})
}

func (c *WSConn) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case v := <-c.out:
			c.mu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			err := c.conn.WriteJSON(v)
			c.mu.Unlock()
			if err != nil {
				c.Close() // stuck/dead client: drop it (it reconnects and replays)
				return
			}
		}
	}
}

// WriteAsync enqueues v for the writer goroutine. It returns false when the
// outbound queue is full — a genuinely stuck client — so the caller can drop it
// instead of blocking. It never blocks the calling (producer) goroutine.
func (c *WSConn) WriteAsync(v any) bool {
	if c.out == nil {
		return false
	}
	select {
	case <-c.ctx.Done():
		return false
	case c.out <- v:
		return true
	default:
		return false
	}
}

// IsNormalClose reports whether err is an ordinary WebSocket disconnect — the
// client closing cleanly (1000), navigating away or reloading the tab (1001),
// or dropping without a close frame (1005). These are expected lifecycle
// events, not failures, so callers should not log them as read errors.
func IsNormalClose(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	)
}

// WriteJSON writes v synchronously, holding the write mutex so concurrent sends
// are safe, and bounding the write with a deadline. Used for handshake/control
// replies on the connection's own goroutine; event delivery uses WriteAsync.
func (c *WSConn) WriteJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.conn.WriteJSON(v)
}

// WriteBinary writes a binary frame synchronously under the write mutex with
// the standard deadline. Terminal byte streams use this instead of the JSON
// event queue: they need frame ordering with backpressure on the producer
// pump, not fire-and-forget enqueueing.
func (c *WSConn) WriteBinary(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.conn.WriteMessage(websocket.BinaryMessage, p)
}

// ReadMessage reads the next frame and returns its websocket message type
// (websocket.TextMessage or websocket.BinaryMessage) alongside the payload.
// For JSON-only protocols prefer ReadJSON.
func (c *WSConn) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

// Close cancels the connection context and closes the underlying websocket.
func (c *WSConn) Close() {
	c.cancel()
	_ = c.conn.Close()
}

// startHeartbeat arms a rolling read deadline pushed forward by each pong and
// pings on a ticker to solicit them (browsers and gorilla clients answer pings
// automatically). Reads fail once the peer stops answering, ending the handler
// loop. WriteControl is safe alongside the other write methods, so no mutex.
func (c *WSConn) startHeartbeat(pongWait, pingInterval time.Duration) {
	c.pongWait = pongWait
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.hbMu.Lock()
		paused := c.hbPaused
		c.hbMu.Unlock()
		if paused {
			return nil
		}
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
					c.Close()
					return
				}
			}
		}
	}()
}

// WSHandlerFunc handles a single upgraded websocket connection.
type WSHandlerFunc func(conn *WSConn)

// HandleWSWithAuth upgrades to WebSocket, then requires the client to send
// {"type":"auth","token":"..."} as the first message — resolved by auth, so a
// static token, a session token and a PAT all work. On success it replies
// with {"type":"auth.ok"} and enters the normal handler loop; on failure it
// closes the connection silently. Failures draw on guard's per-IP budget, the
// same one REST draws on; an exhausted IP is refused before the upgrade. An
// authenticated connection is held in conns (nil: untracked) for as long as
// the handler runs.
func HandleWSWithAuth(handler WSHandlerFunc, auth AuthFunc, guard *AuthGuard, conns *ConnTracker) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if guard.Exhausted(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				protocol.NewErrorResponse(protocol.CodeRateLimited, "too many failed credentials; slow down"))
			return
		}
		ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			logging.Ctx(c.Request.Context()).Error("ws upgrade", "error", err)
			return
		}
		// Cap inbound frame size immediately — before the auth handshake — so an
		// unauthenticated peer cannot OOM the process with a giant frame.
		ws.SetReadLimit(wsMaxMessageBytes)
		ctx, cancel := context.WithCancel(c.Request.Context())
		conn := &WSConn{conn: ws, ctx: ctx, cancel: cancel}

		// Require the auth frame to arrive within a bounded window so an idle,
		// unauthenticated connection can't hold a goroutine and buffer open.
		_ = ws.SetReadDeadline(time.Now().Add(wsAuthDeadline))
		var frame struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		if err := conn.ReadJSON(&frame); err != nil || frame.Type != protocol.EventAuth {
			conn.Close()
			return
		}
		user, err := auth(c.Request.Context(), frame.Token)
		if err != nil {
			guard.Failed(ip)
			conn.Close()
			return
		}
		conn.User = user
		token := frame.Token
		conn.recheck = func(ctx context.Context) (protocol.UserInfo, error) { return auth(ctx, token) }
		// Authenticated: the heartbeat's rolling deadline replaces the auth one
		// (the stream idles between messages by design, so a bare deadline can't
		// stay). The read-size limit stays for the whole connection.
		conn.startHeartbeat(wsPongWait, wsPingInterval)
		_ = conn.WriteJSON(map[string]string{"type": protocol.EventAuthOK})

		conns.add(conn)
		defer conns.remove(conn)
		defer conn.Close()
		handler(conn)
	}
}
