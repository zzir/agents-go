package server

import (
	"context"
	"errors"
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
	// wsOutBuffer bounds a connection's outbound queue: a joiner is attached to
	// EVERY live run of its user with a full replay (512 each); overflow closes it.
	wsOutBuffer = 8 * 512
	// wsWriteTimeout caps a single socket write, so a stalled client cannot
	// block the writer.
	wsWriteTimeout = 15 * time.Second
	// wsMaxMessageBytes bounds an inbound frame, pre-auth included; over it, 1009.
	wsMaxMessageBytes = 1 << 20
	// wsAuthDeadline caps how long an unauthenticated connection may take to
	// send its auth frame.
	wsAuthDeadline = 10 * time.Second
	// wsHandshakeTimeout bounds the upgrade handshake itself.
	wsHandshakeTimeout = 10 * time.Second
	// wsPongWait is the heartbeat's read deadline: no pong within it drops the
	// connection.
	wsPongWait = 60 * time.Second
	// wsPingInterval is how often the heartbeat pings; well under wsPongWait.
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

// WSConn is a websocket connection: a per-connection context, a write mutex,
// and a bounded outbound queue drained by one writer goroutine (StartWriter).
type WSConn struct {
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	out     chan any
	outOnce sync.Once

	// User is who authenticated the connection (HandleWSWithAuth).
	User protocol.UserInfo
	// recheck resolves the connection's credential again — Recheck's half.
	recheck func(context.Context) (protocol.UserInfo, error)

	// The heartbeat's read deadline, and whether PauseHeartbeat lifted it.
	hbMu     sync.Mutex
	hbPaused bool
	pongWait time.Duration
}

// PauseHeartbeat lifts the heartbeat's read deadline for a stretch in which the
// handler will not read (a terminal dialing, an image pull); ResumeHeartbeat
// re-arms it.
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

// Recheck resolves the connection's credential again; when it no longer names
// the same user and role, the connection is closed and false returned. A
// credential the store cannot resolve keeps the connection — invariant 71.
func (c *WSConn) Recheck() bool {
	if c.recheck == nil {
		return true
	}
	u, err := c.recheck(c.ctx)
	if err != nil && !errors.Is(err, ErrUnauthorized) {
		return true
	}
	if err == nil && u.ID == c.User.ID && u.Role == c.User.Role {
		return true
	}
	c.closeWith(websocket.ClosePolicyViolation, "credential no longer valid")
	return false
}

// closeWith sends a close frame carrying code and reason, then closes.
func (c *WSConn) closeWith(code int, reason string) {
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

// CloseAll closes every tracked connection with a going-away frame (shutdown).
func (t *ConnTracker) CloseAll(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	var conns []*WSConn
	for _, set := range t.byUser {
		for c := range set {
			conns = append(conns, c)
		}
	}
	t.mu.Unlock()
	for _, c := range conns {
		c.closeWith(websocket.CloseGoingAway, reason)
	}
}

// CloseForUser closes every connection the user holds, with reason; each
// client reconnects and authenticates afresh.
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
		c.closeWith(websocket.ClosePolicyViolation, reason)
	}
}

// Context returns the per-connection context, cancelled when the connection closes.
func (c *WSConn) Context() context.Context { return c.ctx }

// ReadJSON reads the next JSON message from the connection into v.
func (c *WSConn) ReadJSON(v any) error {
	return c.conn.ReadJSON(v)
}

// StartWriter launches the outbound writer goroutine (idempotent); after this,
// WriteAsync delivers events without blocking on the network.
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

// WriteAsync enqueues v for the writer goroutine without blocking; false when
// the outbound queue is full (a stuck client), so the caller drops it.
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

// IsNormalClose reports whether err is an ordinary WebSocket disconnect (1000,
// 1001, 1005), which callers should not log as a read error.
func IsNormalClose(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	)
}

// WriteJSON writes v synchronously under the write mutex with a deadline, for
// handshake/control replies on the connection's own goroutine; events use WriteAsync.
func (c *WSConn) WriteJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.conn.WriteJSON(v)
}

// WriteBinary writes a binary frame synchronously under the write mutex with
// the standard deadline; terminal byte streams need ordering with backpressure,
// not the queue.
func (c *WSConn) WriteBinary(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return c.conn.WriteMessage(websocket.BinaryMessage, p)
}

// ReadMessage reads the next frame and returns its websocket message type
// (text or binary) alongside the payload; JSON-only protocols use ReadJSON.
func (c *WSConn) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

// Close cancels the connection context and closes the underlying websocket.
func (c *WSConn) Close() {
	c.cancel()
	_ = c.conn.Close()
}

// startHeartbeat arms a rolling read deadline pushed forward by each pong and
// pings on a ticker; WriteControl is safe alongside the other writers, no mutex.
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

// HandleWSWithAuth upgrades to WebSocket and requires {"type":"auth","token":...}
// as the first message, resolved by auth; it answers {"type":"auth.ok"} and runs
// handler, or closes silently. Failures draw on guard's per-IP budget; conns
// (nil: untracked) holds the connection while handler runs.
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
		// Cap inbound frame size before the auth handshake.
		ws.SetReadLimit(wsMaxMessageBytes)
		ctx, cancel := context.WithCancel(c.Request.Context())
		conn := &WSConn{conn: ws, ctx: ctx, cancel: cancel}

		// The auth frame must arrive within a bounded window.
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
			if !errors.Is(err, ErrUnauthorized) {
				logging.Ctx(c.Request.Context()).Error("credential check", "error", err)
				conn.closeWith(websocket.CloseTryAgainLater, "credential could not be checked")
				return
			}
			guard.Failed(ip)
			conn.Close()
			return
		}
		conn.User = user
		token := frame.Token
		conn.recheck = func(ctx context.Context) (protocol.UserInfo, error) { return auth(ctx, token) }
		// Authenticated: the heartbeat's rolling deadline replaces the auth one
		// (the stream idles by design); the read-size limit stays.
		conn.startHeartbeat(wsPongWait, wsPingInterval)
		_ = conn.WriteJSON(map[string]string{"type": protocol.EventAuthOK})

		conns.add(conn)
		defer conns.remove(conn)
		defer conn.Close()
		handler(conn)
	}
}
