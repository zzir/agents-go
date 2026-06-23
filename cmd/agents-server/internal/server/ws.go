package server

import (
	"context"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
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
