package handler

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

const testWSToken = "t"

// dialWS connects and authenticates one WebSocket client against srv.
func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]string{"type": protocol.EventAuth, "token": testWSToken}); err != nil {
		t.Fatalf("send auth: %v", err)
	}
	var ack protocol.Envelope
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&ack); err != nil || ack.Type != protocol.EventAuthOK {
		t.Fatalf("auth ack: type=%q err=%v", ack.Type, err)
	}
	return conn
}

// readUntil reads envelopes until one of the wanted type arrives, failing the
// test on timeout. Other event types are skipped.
func readUntil(t *testing.T, conn *websocket.Conn, typ string) protocol.Envelope {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if env.Type == typ {
			return env
		}
	}
}

// Run events are a broadcast bus: a run started by one connection must stream
// to EVERY authenticated connection — a watcher that never sent run.create
// (browser B), including its user prompt via run.started's input — and a
// connection that joins mid-run must be attached with a replay. This is the
// contract that lets two browsers watch the same session live.
func TestRunEventsBroadcastToAllConnections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	deps := &bridge.AgentDeps{
		AgentConfigs: store.NewAgentConfigStore(db),
		Sessions:     sessions,
		Traces:       store.NewTraceStore(db),
	}
	runner := bridge.NewRunner(t.Context(), db, deps)
	wsh := NewWSHandler(runner, sessions, store.NewPendingApprovalStore(db))
	engine := newTestEngine()
	engine.GET("/ws", server.HandleWSWithAuth(wsh.Handle, testAuthFunc(testWSToken), nil))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	watcher := dialWS(t, srv) // browser B: never sends run.create
	sender := dialWS(t, srv)  // browser A

	create, _ := json.Marshal(protocol.RunCreate{SessionID: sess.ID, Input: "hello from A"})
	if err := sender.WriteJSON(protocol.Envelope{Type: protocol.EventRunCreate, Payload: create}); err != nil {
		t.Fatal(err)
	}

	// The nonexistent agent config fails the build, so the run's whole event
	// stream is run.started + run.error — both must reach the WATCHER.
	started := readUntil(t, watcher, protocol.EventRunStarted)
	var sp protocol.RunStarted
	if err := json.Unmarshal(started.Payload, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.SessionID != sess.ID || sp.Input != "hello from A" {
		t.Fatalf("watcher run.started = %+v, want session %s with the sender's input", sp, sess.ID)
	}
	readUntil(t, watcher, protocol.EventRunError)
	// The sender sees its own run too, via the same broadcast path.
	readUntil(t, sender, protocol.EventRunStarted)

	// A connection that joins later must be attached to runs it never heard
	// about. A config-error run terminates instantly, so it can't linger in
	// the live set for this test — instead prove the wiring both ways:
	// a third connection joining now cleanly receives the NEXT run's stream.
	watcher2 := dialWS(t, srv)
	create2, _ := json.Marshal(protocol.RunCreate{SessionID: sess.ID, Input: "second"})
	if err := sender.WriteJSON(protocol.Envelope{Type: protocol.EventRunCreate, Payload: create2}); err != nil {
		t.Fatal(err)
	}
	started2 := readUntil(t, watcher2, protocol.EventRunStarted)
	var sp2 protocol.RunStarted
	_ = json.Unmarshal(started2.Payload, &sp2)
	if sp2.Input != "second" {
		t.Fatalf("late-joining watcher got %+v, want the second run", sp2)
	}
	// The mid-run attach itself (register → LiveRunIDs → Subscribe-with-replay)
	// is covered by TestRunHubLiveRunIDs plus the hub's replay contract that
	// the SSE handler already depends on.

	// The broadcast hook reaches the connections a run's stream does not: a
	// connection that joined AFTER the run (never attached to it) hears the
	// broadcast; one attached to the run — which already carried the event —
	// does not hear it twice.
	// Once the second run has left the live set, a connection dialing now is
	// NOT attached to it — the situation of a browser joining after a run
	// paused on an approval.
	readUntil(t, watcher2, protocol.EventRunError)
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(runner.Hub().LiveRunIDs(), sp2.RunID) {
		if time.Now().After(deadline) {
			t.Fatal("the second run never left the live set")
		}
		time.Sleep(10 * time.Millisecond)
	}
	late := dialWS(t, srv)
	// The auth ack is written before the connection is registered on the bus;
	// wait for the registration the broadcast walks.
	deadline = time.Now().Add(5 * time.Second)
	for {
		wsh.registry.mu.Lock()
		n := len(wsh.registry.conns)
		wsh.registry.mu.Unlock()
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry holds %d connections, want 4", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	upd, _ := json.Marshal(protocol.TaskUpdated{TaskID: "t1", ParentSessionID: sess.ID, Status: "cancelled"})
	runner.OnBroadcast(&protocol.Envelope{Type: protocol.EventTaskUpdated, Payload: upd}, sp2.RunID, sess.ID)
	got := readUntil(t, late, protocol.EventTaskUpdated)
	var tu protocol.TaskUpdated
	_ = json.Unmarshal(got.Payload, &tu)
	if tu.TaskID != "t1" {
		t.Fatalf("late connection got %+v, want the broadcast task update", tu)
	}
	_ = watcher2.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	for {
		var env protocol.Envelope
		if err := watcher2.ReadJSON(&env); err != nil {
			break // timed out: nothing more — the attached connection was skipped
		}
		if env.Type == protocol.EventTaskUpdated {
			t.Fatal("a connection attached to the run must not hear the broadcast as well")
		}
	}
}
