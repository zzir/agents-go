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
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
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
// closes the connection silently.
func HandleWSWithAuth(handler WSHandlerFunc, auth AuthFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		if _, err := auth(c.Request.Context(), frame.Token); err != nil {
			conn.Close()
			return
		}
		// Authenticated: the heartbeat's rolling deadline replaces the auth one
		// (the stream idles between messages by design, so a bare deadline can't
		// stay — but no deadline at all left half-open connections pinned until
		// TCP keepalive). The read-size limit stays for the whole connection.
		conn.startHeartbeat(wsPongWait, wsPingInterval)
		_ = conn.WriteJSON(map[string]string{"type": protocol.EventAuthOK})

		defer conn.Close()
		handler(conn)
	}
}
