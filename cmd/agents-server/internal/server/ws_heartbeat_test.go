package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// heartbeatServer upgrades, starts a fast heartbeat, and blocks reading until
// the connection dies; closed receives the read error that ended it.
func heartbeatServer(t *testing.T, pongWait, pingInterval time.Duration) (wsURL string, closed chan error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	closed = make(chan error, 1)
	engine := gin.New()
	engine.GET("/ws", func(c *gin.Context) {
		ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(c.Request.Context())
		conn := &WSConn{conn: ws, ctx: ctx, cancel: cancel}
		defer conn.Close()
		conn.startHeartbeat(pongWait, pingInterval)
		for {
			var v map[string]any
			if err := conn.ReadJSON(&v); err != nil {
				closed <- err
				return
			}
		}
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", closed
}

// A peer that stops answering pings — never reading, like a client behind a
// NAT that idled out — must be dropped by the rolling read deadline instead of
// pinning its goroutine until TCP keepalive.
func TestWSHeartbeatDropsUnresponsivePeer(t *testing.T) {
	wsURL, closed := heartbeatServer(t, 300*time.Millisecond, 100*time.Millisecond)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	defer conn.Close()
	// Never read: pings go unprocessed, so no pongs are sent.
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server kept an unresponsive connection past the pong deadline")
	}
}

// A healthy client — one that reads, so gorilla's default ping handler answers
// with pongs — must ride through many heartbeat periods untouched.
func TestWSHeartbeatKeepsResponsivePeer(t *testing.T) {
	wsURL, closed := heartbeatServer(t, 300*time.Millisecond, 100*time.Millisecond)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	defer conn.Close()
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-closed:
		t.Fatalf("server dropped a responsive connection: %v", err)
	case <-time.After(1200 * time.Millisecond): // four pong deadlines
	}
}

// A handler that will not read for a while — a terminal dialing its host —
// pauses the heartbeat: the deadline no pong could have extended does not
// end the connection, and the first read afterwards succeeds. Resumed, the
// deadline guards again.
func TestWSHeartbeatPausesForASlowHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const pongWait, ping = 200 * time.Millisecond, 50 * time.Millisecond
	read := make(chan error, 1)
	engine := gin.New()
	engine.GET("/ws", func(c *gin.Context) {
		ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(c.Request.Context())
		conn := &WSConn{conn: ws, ctx: ctx, cancel: cancel}
		defer conn.Close()
		conn.startHeartbeat(pongWait, ping)
		conn.PauseHeartbeat()
		time.Sleep(3 * pongWait) // the slow open: no read, so no pong is processed
		conn.ResumeHeartbeat()
		var v map[string]any
		read <- conn.ReadJSON(&v)
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	defer conn.Close()
	// Answer pings as a browser would; send the frame once the handler's
	// pause has surely elapsed.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	time.Sleep(3*pongWait + 50*time.Millisecond)
	if err := conn.WriteJSON(map[string]string{"type": "terminal.input"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("the first read after a paused heartbeat failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never read the frame")
	}
}
