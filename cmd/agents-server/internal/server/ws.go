package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

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
)

var upgrader = websocket.Upgrader{
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

// Close cancels the connection context and closes the underlying websocket.
func (c *WSConn) Close() {
	c.cancel()
	_ = c.conn.Close()
}

// WSHandlerFunc handles a single upgraded websocket connection.
type WSHandlerFunc func(conn *WSConn)

// HandleWS upgrades an HTTP request to a WebSocket and runs handler with the connection.
func HandleWS(handler WSHandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			zerolog.Ctx(c.Request.Context()).Error().Err(err).Msg("ws upgrade")
			return
		}
		ctx, cancel := context.WithCancel(c.Request.Context())
		conn := &WSConn{conn: ws, ctx: ctx, cancel: cancel}
		defer conn.Close()
		handler(conn)
	}
}

// HandleWSWithAuth upgrades to WebSocket, then requires the client to send
// {"type":"auth","token":"..."} as the first message. On success it replies
// with {"type":"auth.ok"} and enters the normal handler loop; on failure it
// closes the connection silently.
func HandleWSWithAuth(handler WSHandlerFunc, token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			zerolog.Ctx(c.Request.Context()).Error().Err(err).Msg("ws upgrade")
			return
		}
		ctx, cancel := context.WithCancel(c.Request.Context())
		conn := &WSConn{conn: ws, ctx: ctx, cancel: cancel}

		var auth struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		if err := conn.ReadJSON(&auth); err != nil || auth.Type != protocol.EventAuth ||
			subtle.ConstantTimeCompare([]byte(auth.Token), []byte(token)) != 1 {
			conn.Close()
			return
		}
		_ = conn.WriteJSON(map[string]string{"type": protocol.EventAuthOK})

		defer conn.Close()
		handler(conn)
	}
}
