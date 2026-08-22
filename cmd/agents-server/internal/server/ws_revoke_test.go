package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// dialAuthed opens a WebSocket to srv and authenticates with token.
func dialAuthed(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]string{"type": "auth", "token": token}); err != nil {
		t.Fatalf("send auth: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack map[string]string
	if err := conn.ReadJSON(&ack); err != nil || ack["type"] != "auth.ok" {
		t.Fatalf("auth.ok: %v %v", ack, err)
	}
	return conn
}

// expectPolicyClose reads until the peer's close frame and checks its code.
func expectPolicyClose(t *testing.T, conn *websocket.Conn, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("%s: want a 1008 close, got %v", what, err)
	}
}

// A frame acts as the credential that opened the connection, so the
// credential is resolved again before each one: once revoked — or once the
// role changes — the next frame closes the connection with 1008 instead of
// acting, and a credential still valid keeps acting.
func TestWSRecheckClosesARevokedConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var revoked, demoted atomic.Bool
	auth := func(_ context.Context, bearer string) (protocol.UserInfo, error) {
		if bearer != "tok" || revoked.Load() {
			return protocol.UserInfo{}, errors.New("unauthorized")
		}
		role := "admin"
		if demoted.Load() {
			role = "member"
		}
		return protocol.UserInfo{ID: "u1", Role: role}, nil
	}
	acted := make(chan struct{}, 8)
	engine := gin.New()
	engine.GET("/ws", HandleWSWithAuth(func(conn *WSConn) {
		for {
			var frame map[string]string
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if !conn.Recheck() {
				return
			}
			acted <- struct{}{}
		}
	}, auth, nil, nil))
	srv := httptest.NewServer(engine)
	defer srv.Close()

	conn := dialAuthed(t, srv, "tok")
	_ = conn.WriteJSON(map[string]string{"type": "ping"})
	select {
	case <-acted:
	case <-time.After(2 * time.Second):
		t.Fatal("a valid credential's frame must act")
	}
	revoked.Store(true)
	_ = conn.WriteJSON(map[string]string{"type": "ping"})
	expectPolicyClose(t, conn, "frame after revocation")
	select {
	case <-acted:
		t.Fatal("a frame after revocation acted")
	default:
	}

	revoked.Store(false)
	conn = dialAuthed(t, srv, "tok")
	demoted.Store(true)
	_ = conn.WriteJSON(map[string]string{"type": "ping"})
	expectPolicyClose(t, conn, "frame after a role change")
}

// CloseForUser ends every tracked connection of one user, and no other's,
// with a 1008 close frame.
func TestConnTrackerClosesAUsersConnections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	auth := func(_ context.Context, bearer string) (protocol.UserInfo, error) {
		switch bearer {
		case "a":
			return protocol.UserInfo{ID: "ua"}, nil
		case "b":
			return protocol.UserInfo{ID: "ub"}, nil
		}
		return protocol.UserInfo{}, errors.New("unauthorized")
	}
	tracker := NewConnTracker()
	engine := gin.New()
	engine.GET("/ws", HandleWSWithAuth(func(conn *WSConn) { <-conn.Context().Done() }, auth, nil, tracker))
	srv := httptest.NewServer(engine)
	defer srv.Close()

	a1, a2, b := dialAuthed(t, srv, "a"), dialAuthed(t, srv, "a"), dialAuthed(t, srv, "b")
	// The handler registers after auth.ok is written; wait for both of a's.
	deadline := time.Now().Add(2 * time.Second)
	for {
		tracker.mu.Lock()
		n := len(tracker.byUser["ua"])
		tracker.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tracker.CloseForUser("ua", "signed out")
	expectPolicyClose(t, a1, "a's first connection")
	expectPolicyClose(t, a2, "a's second connection")
	_ = b.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := b.ReadMessage(); !errors.Is(err, context.DeadlineExceeded) && !isTimeout(err) {
		t.Fatalf("b's connection must stay open, got %v", err)
	}
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}
