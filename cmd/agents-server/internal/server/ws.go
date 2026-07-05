package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
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

// WSConn is a websocket connection with a per-connection context and a write mutex for concurrent sends.
type WSConn struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

// Context returns the per-connection context, cancelled when the connection closes.
func (c *WSConn) Context() context.Context { return c.ctx }

// ReadJSON reads the next JSON message from the connection into v.
func (c *WSConn) ReadJSON(v any) error {
	return c.conn.ReadJSON(v)
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

// WriteJSON writes v as a JSON message, holding the write mutex so concurrent sends are safe.
func (c *WSConn) WriteJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
		if err := conn.ReadJSON(&auth); err != nil || auth.Type != "auth" ||
			subtle.ConstantTimeCompare([]byte(auth.Token), []byte(token)) != 1 {
			conn.Close()
			return
		}
		_ = conn.WriteJSON(map[string]string{"type": "auth.ok"})

		defer conn.Close()
		handler(conn)
	}
}
