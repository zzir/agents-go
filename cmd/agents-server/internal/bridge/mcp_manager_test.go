package bridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/mcp"
)

// TestFinishConnectDiscardsSupersededHandshake locks: a handshake that
// completes AFTER its config was reconciled away (Disconnect) must have its
// fresh connection discarded, not installed — otherwise a reconfigured or
// disabled server stays connected with stale config. Disconnect also cancels
// the in-flight handshake so it releases the connect slot promptly.
func TestFinishConnectDiscardsSupersededHandshake(t *testing.T) {
	m := NewMcpManager(context.Background(), nil)

	done, hctx, gen, err := m.beginConnect(context.Background(), "srv1")
	if err != nil || done {
		t.Fatalf("beginConnect: done=%v err=%v", done, err)
	}

	// Reconcile drops the connection mid-handshake.
	_ = m.Disconnect("srv1")
	if hctx.Err() == nil {
		t.Fatal("Disconnect must cancel the in-flight handshake context ")
	}

	// The stale handshake still "succeeds" and hands finishConnect a server; the
	// superseded generation must cause it to be discarded and the slot released.
	if err := m.finishConnect("srv1", gen, &mcp.Server{}, nil); err != nil {
		t.Fatalf("finishConnect (superseded): %v", err)
	}
	if m.IsConnected("srv1") {
		t.Fatal("a superseded handshake's server was installed ")
	}
	if m.IsConnecting("srv1") {
		t.Fatal("the connect slot was not released after a superseded finishConnect")
	}

	// A fresh connect proceeds and its current-generation result installs.
	done, _, gen2, err := m.beginConnect(context.Background(), "srv1")
	if err != nil || done {
		t.Fatalf("re-beginConnect: done=%v err=%v", done, err)
	}
	if err := m.finishConnect("srv1", gen2, &mcp.Server{}, nil); err != nil {
		t.Fatalf("finishConnect (fresh): %v", err)
	}
	if !m.IsConnected("srv1") {
		t.Fatal("a current-generation handshake should install its server")
	}
}

// A handshake in flight must not block state reads. beginConnect claims the
// slot and releases the manager lock (the handshake runs outside it), so
// Get/IsConnected/Disconnect stay responsive while a slow server connects.
func TestMcpManagerConnectDoesNotBlockReads(t *testing.T) {
	m := NewMcpManager(context.Background(), nil)

	// Simulate a claimed-but-not-finished connect (a slow handshake in flight).
	done, _, gen, err := m.beginConnect(context.Background(), "srv1")
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

	// The in-flight handshake is observable: this is what the API's
	// "connecting" status is derived from.
	if !m.IsConnecting("srv1") {
		t.Fatal("IsConnecting should be true while a handshake is in flight")
	}
	if m.IsConnecting("other") {
		t.Fatal("IsConnecting should be false for a server with no handshake")
	}

	// A concurrent Connect for the same server is deduped, not run twice.
	if _, _, _, err := m.beginConnect(context.Background(), "srv1"); !errors.Is(err, ErrConnectInProgress) {
		t.Fatalf("second beginConnect: err = %v, want ErrConnectInProgress", err)
	}

	// Finishing the claim clears the in-progress flag; a later beginConnect for
	// the (still unconnected) server can proceed again.
	if err := m.finishConnect("srv1", gen, nil, errors.New("handshake failed")); err == nil {
		t.Fatal("finishConnect should surface the handshake error")
	}
	if m.IsConnecting("srv1") {
		t.Fatal("IsConnecting should be false after the handshake finished")
	}
	if done, _, _, err := m.beginConnect(context.Background(), "srv1"); done || err != nil {
		t.Fatalf("after a failed connect, beginConnect should be claimable again: done=%v err=%v", done, err)
	}
}

// LiveRunIDs feeds the WS broadcast bus: a freshly connected browser is
// attached to exactly the runs still executing — finished and interrupted
// runs leave the set (resume re-adds and re-attaches via OnRunAttach).
func TestRunHubLiveRunIDs(t *testing.T) {
	h := NewRunHub(t.Context())
	if got := h.LiveRunIDs(); len(got) != 0 {
		t.Fatalf("empty hub: got %v", got)
	}
	if _, _, err := h.register("r1", "s1", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.register("r2", "s2", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if got := h.LiveRunIDs(); len(got) != 2 {
		t.Fatalf("two live runs: got %v", got)
	}
	h.finish("r1", false)
	got := h.LiveRunIDs()
	if len(got) != 1 || got[0] != "r2" {
		t.Fatalf("after finish: got %v, want [r2]", got)
	}
	h.finish("r2", true) // interrupted also leaves the live set
	if got := h.LiveRunIDs(); len(got) != 0 {
		t.Fatalf("after interrupt: got %v", got)
	}
}

// IsAuthorizing tracks the interactive OAuth attempt for a server id and is
// safe on a nil coordinator (handlers constructed without OAuth in tests).
func TestOAuthCoordinatorIsAuthorizing(t *testing.T) {
	var nilC *OAuthCoordinator
	if nilC.IsAuthorizing("x") {
		t.Fatal("nil coordinator should report no flows")
	}

	c := NewOAuthCoordinator(nil)
	if c.IsAuthorizing("srv1") {
		t.Fatal("no flow started yet")
	}
	a := &oauthAttempt{cancel: func() {}, done: make(chan struct{})}
	c.mu.Lock()
	c.inflight["srv1"] = a
	c.mu.Unlock()
	if !c.IsAuthorizing("srv1") {
		t.Fatal("flow in progress should be reported")
	}
	c.clearInflight("srv1", a)
	if c.IsAuthorizing("srv1") {
		t.Fatal("finished flow should no longer be reported")
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
	var fastHit atomic.Int32
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fastHit.Add(1)
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

	mgr := NewMcpManager(ctx, settings.NewReader(store.NewSettingStore(db)))
	go ConnectEnabledMcpServers(ctx, mgr, mcpStore, nil)

	// The reachable server must be reached well within the hung server's 30s
	// handshake timeout — proving the two connects run concurrently.
	deadline := time.After(8 * time.Second)
	for fastHit.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("reachable server was not connected while another hung — auto-connect is serialized")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// syncBuffer collects log output written by a background goroutine while the
// test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Reconcile's reconnect runs in the background, long after the config write
// answered its client: a failure there has nowhere to surface but the log, so
// it must not be swallowed.
func TestReconcileLogsFailedReconnect(t *testing.T) {
	var logs syncBuffer
	ctx := logging.Into(t.Context(), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	m := NewMcpManager(ctx, nil)

	m.Reconcile(&store.McpServerConfig{
		ID: store.NewID(), Name: "broken", TransportType: "stdio", Enabled: true,
		Config: []byte(`{"command":"agents-server-no-such-executable"}`),
	}, nil)

	deadline := time.After(10 * time.Second)
	for !strings.Contains(logs.String(), "mcp reconnect after config change failed") {
		select {
		case <-deadline:
			t.Fatalf("a failed background reconnect left no log trace; got %q", logs.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// buildMcpOptions is the single assembly point for an MCP connection's options:
// it sets the per-server tool-name prefix and maps the stored retry policy.
func TestBuildMcpOptions(t *testing.T) {
	opts := buildMcpOptions("github", store.McpRetryConfig{MaxRetryAttempts: 3, RetryBackoffMs: 500}, true)
	if opts.ToolNamePrefix != "github__" {
		t.Errorf("prefix = %q, want github__", opts.ToolNamePrefix)
	}
	if opts.MaxRetryAttempts != 3 {
		t.Errorf("MaxRetryAttempts = %d, want 3", opts.MaxRetryAttempts)
	}
	if opts.RetryBackoffBase != 500*time.Millisecond {
		t.Errorf("RetryBackoffBase = %v, want 500ms", opts.RetryBackoffBase)
	}
	if !opts.UseStructuredContent {
		t.Error("UseStructuredContent = false, want true")
	}

	// Defaults: no retries, no backoff override, content blocks.
	def := buildMcpOptions("x", store.McpRetryConfig{}, false)
	if def.MaxRetryAttempts != 0 || def.RetryBackoffBase != 0 {
		t.Errorf("default opts = %+v, want zero retry/backoff", def)
	}
}
