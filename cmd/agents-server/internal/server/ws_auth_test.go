package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// an inbound frame larger than the read limit must be rejected before the
// auth handshake even completes — gorilla defaults to no limit, so without the
// cap an unauthenticated peer could stream an arbitrarily large frame into
// memory. The auth handler must never run for such a connection.
func TestWSAuthReadLimitRejectsOversizedFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reached := make(chan struct{}, 1)
	engine := gin.New()
	engine.GET("/ws", HandleWSWithAuth(func(conn *WSConn) {
		select {
		case reached <- struct{}{}:
		default:
		}
	}, staticAuth("tok")))
	srv := httptest.NewServer(engine)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	defer conn.Close()

	big := make([]byte, wsMaxMessageBytes+1024)
	for i := range big {
		big[i] = 'a'
	}
	// The server trips the read limit mid-frame and closes the connection, so the
	// write may fail with a broken pipe (already proof it was rejected) or succeed
	// and the subsequent read fails with a close. Both outcomes are acceptable;
	// what matters is that the auth handler never runs.
	if err := conn.WriteMessage(websocket.TextMessage, big); err == nil {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("connection was not closed after an oversized frame")
		}
	}
	select {
	case <-reached:
		t.Fatal("auth handler ran despite an oversized pre-auth frame")
	case <-time.After(300 * time.Millisecond):
	}
}

// A normal-sized valid auth frame still authenticates and reaches the handler —
// the read limit must not interfere with legitimate traffic.
func TestWSAuthSucceedsWithinReadLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reached := make(chan struct{}, 1)
	engine := gin.New()
	engine.GET("/ws", HandleWSWithAuth(func(conn *WSConn) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-conn.Context().Done()
	}, staticAuth("tok")))
	srv := httptest.NewServer(engine)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	defer conn.Close()

	if err := conn.WriteJSON(map[string]string{"type": "auth", "token": "tok"}); err != nil {
		t.Fatalf("send auth: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack map[string]string
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read auth.ok: %v", err)
	}
	if ack["type"] != "auth.ok" {
		t.Fatalf("ack type = %q, want auth.ok", ack["type"])
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not reached after successful auth")
	}
}
