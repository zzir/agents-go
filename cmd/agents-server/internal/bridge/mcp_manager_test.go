package bridge

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A handshake in flight must not block state reads. beginConnect claims the
// slot and releases the manager lock (the handshake runs outside it), so
// Get/IsConnected/Disconnect stay responsive while a slow server connects.
func TestMcpManagerConnectDoesNotBlockReads(t *testing.T) {
	m := NewMcpManager(context.Background(), nil)

	// Simulate a claimed-but-not-finished connect (a slow handshake in flight).
	done, err := m.beginConnect("srv1")
	if err != nil || done {
		t.Fatalf("beginConnect: done=%v err=%v", done, err)
	}

	// These must return promptly, not deadlock behind the in-flight handshake.
	got := make(chan struct{})
	go func() {
		_ = m.Get("srv1")
		_ = m.IsConnected("srv1")
		_ = m.Get("other")
		_ = m.Disconnect("srv1") // not connected yet -> no-op
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("state reads blocked while a connect was in flight")
	}

	// A concurrent Connect for the same server is deduped, not run twice.
	if _, err := m.beginConnect("srv1"); !errors.Is(err, ErrConnectInProgress) {
		t.Fatalf("second beginConnect: err = %v, want ErrConnectInProgress", err)
	}

	// Finishing the claim clears the in-progress flag; a later beginConnect for
	// the (still unconnected) server can proceed again.
	if err := m.finishConnect("srv1", nil, errors.New("handshake failed")); err == nil {
		t.Fatal("finishConnect should surface the handshake error")
	}
	if done, err := m.beginConnect("srv1"); done || err != nil {
		t.Fatalf("after a failed connect, beginConnect should be claimable again: done=%v err=%v", done, err)
	}
}

// Startup auto-connect must connect enabled servers concurrently: one hung
// server must not stall the others. With a hung server first and a reachable
// server second, the reachable one's connect is attempted immediately —
// sequential auto-connect would leave it untouched until the hung server hit
// its 30s handshake timeout.
func TestConnectEnabledMcpServersConcurrent(t *testing.T) {
	ctx := context.Background()

	// A raw TCP listener that accepts but never responds: the MCP HTTP
	// handshake connects and blocks reading the response (a "hung" server).
	hung, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer hung.Close()
	go func() {
		for {
			conn, err := hung.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	// A server that responds immediately (fails the MCP handshake fast) and
	// records that its connect was attempted.
	var fastHit int32
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&fastHit, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer fast.Close()

	db := newTestDB(t)
	mcpStore := store.NewMcpServerStore(db)
	mk := func(name, endpoint string) {
		cfg := &store.McpServerConfig{
			ID: store.NewID(), Name: name, TransportType: "streamable_http", Enabled: true,
			Config: []byte(`{"endpoint":"` + endpoint + `"}`),
		}
		if err := mcpStore.Create(ctx, cfg); err != nil {
			t.Fatalf("create mcp %s: %v", name, err)
		}
	}
	mk("hung", "http://"+hung.Addr().String())
	mk("fast", fast.URL)

	mgr := NewMcpManager(ctx, store.NewSettingStore(db))
	go ConnectEnabledMcpServers(ctx, mgr, mcpStore, nil)

	// The reachable server must be reached well within the hung server's 30s
	// handshake timeout — proving the two connects run concurrently.
	deadline := time.After(8 * time.Second)
	for atomic.LoadInt32(&fastHit) == 0 {
		select {
		case <-deadline:
			t.Fatal("reachable server was not connected while another hung — auto-connect is serialized")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
