package mcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

// swapHandler serves whichever MCP server is currently installed, so a test can
// replace the server behind a stable endpoint — a restart, from the client's
// point of view, and the most faithful way to kill a connection: the session id
// the client holds is no longer one the server knows.
type swapHandler struct {
	mu sync.Mutex
	h  http.Handler
}

func (s *swapHandler) set(h http.Handler) {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
}

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	h.ServeHTTP(w, r)
}

// mcpHandlerWith builds a fresh MCP server exposing one named tool.
func mcpHandlerWith(toolName string) http.Handler {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "healer", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: toolName, Description: "answer"},
		func(context.Context, *mcpsdk.CallToolRequest, pingArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}},
			}, nil, nil
		})
	return mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true})
}

// TestConnectionHealsAfterTheServerRestarts locks the recovery half: nothing
// in the go-sdk reconnects, so without Redial the first connection failure
// would be permanent (decisions §5.21).
func TestConnectionHealsAfterTheServerRestarts(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)

	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{
			Redial: func(context.Context) (mcpsdk.Transport, error) {
				return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
			},
		})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc := agents.NewRunContext(nil)
	if _, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "a"}); err != nil {
		t.Fatalf("list tools: %v", err)
	}

	// The server restarts behind the same address, with a different tool, so a
	// list served from the old connection would be visibly the old one.
	swap.set(mcpHandlerWith("pong"))

	// The call that discovers the death may still fail — a connection cannot be
	// healed before anyone knows it is gone. What must not happen is the death
	// being permanent, so ask until the deadline, not exactly once.
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for {
		server.InvalidateToolsCache()
		tools, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "a"})
		if err == nil {
			if len(tools) != 1 || tools[0].Name != "pong" {
				t.Fatalf("healed onto the wrong server: %v", toolNames(tools))
			}
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("the connection never healed after the server restarted: %v", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestConnectionStaysDeadWithoutRedial is the other side of the same contract:
// healing is opt-in, since only the caller that owns the configuration knows
// how to rebuild the transport; without a Redial the dead connection says so.
func TestConnectionStaysDeadWithoutRedial(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)

	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, Options{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc := agents.NewRunContext(nil)
	if _, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "a"}); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	swap.set(mcpHandlerWith("pong"))

	// Two calls: the one that kills the connection, and the one that proves it
	// stayed dead.
	deadline := time.Now().Add(10 * time.Second)
	for {
		server.InvalidateToolsCache()
		if _, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "a"}); err != nil {
			return // dead, as documented
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection was expected to stay broken without a Redial")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func toolNames(tools []*agents.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestSessionOutlivesItsConnectContext guards the startup path: a session must
// outlive the context it was CONNECTED under, or the watcher would read the
// handshake's cancel as the connection dying and heal-loop from startup.
func TestSessionOutlivesItsConnectContext(t *testing.T) {
	endpoint := httptest.NewServer(mcpHandlerWith("ping"))
	t.Cleanup(endpoint.Close)

	var dials atomic.Int32
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	server, err := NewWithTransport(cctx, "probe",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{
			Redial: func(context.Context) (mcpsdk.Transport, error) {
				dials.Add(1)
				return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	cancel() // what ConnectEnabledMcpServers does right after Connect returns
	time.Sleep(1 * time.Second)

	rc := agents.NewRunContext(nil)
	if _, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "a"}); err != nil {
		t.Errorf("ListTools after the connect context was cancelled: %v", err)
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("the watcher redialed %d times on a healthy connection whose connect context was cancelled", n)
	}
}

// TestRedialContextOutlivesTheCall locks what Options.Redial promises: the
// context it receives is the CONNECTION's, so a stdio server's subprocess
// bound to it lives as long as the connection does.
func TestRedialContextOutlivesTheCall(t *testing.T) {
	endpoint := httptest.NewServer(mcpHandlerWith("ping"))
	t.Cleanup(endpoint.Close)

	var got context.Context
	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{
			Redial: func(ctx context.Context) (mcpsdk.Transport, error) {
				got = ctx
				return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
			},
		})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	if healed, _ := server.redial(server.session.Load()); !healed {
		t.Fatal("redial did not replace the session")
	}
	if got == nil {
		t.Fatal("Redial was never called")
	}
	if err := got.Err(); err != nil {
		t.Fatalf("the redial context was cancelled on return: %v", err)
	}

	// It ends with the server, which is the other half of "the connection's own".
	_ = server.Close()
	if got.Err() == nil {
		t.Fatal("the redial context outlived Close")
	}
}

// TestThrottledWatcherRetriesAfterTheCooldown locks the other half of the
// throttle: a connection that dies inside the redial cooldown — healed, then
// dead again at once — is not left dead until a caller trips over it. The
// watcher waits the cooldown out and dials once more (spec §2.16).
func TestThrottledWatcherRetriesAfterTheCooldown(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)

	var dials atomic.Int32
	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{
			Redial: func(context.Context) (mcpsdk.Transport, error) {
				dials.Add(1)
				return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
			},
		})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc := agents.NewRunContext(nil)
	agent := &agents.Agent{Name: "a"}
	if _, err := server.ListTools(context.Background(), rc, agent); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	first := server.session.Load()

	// As if a heal had just landed: the death that follows falls inside the
	// cooldown, so the first redial it asks for is throttled.
	forced := time.Now()
	server.dialMu.Lock()
	server.lastDial = forced
	server.cooldown = redialCooldown
	server.dialMu.Unlock()

	// The restart kills the connection on the request that discovers it — made
	// on the raw session, so no caller waits for the heal: from here only the
	// watcher can heal the slot.
	swap.set(mcpHandlerWith("pong"))
	killSession(t, first)

	for deadline := time.Now().Add(10 * time.Second); server.session.Load() == first; {
		if time.Now().After(deadline) {
			t.Fatalf("the watcher never redialed after the cooldown (dials = %d)", dials.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if since := time.Since(forced); since < redialCooldown {
		t.Errorf("healed %v after the forced dial, inside the %v cooldown", since, redialCooldown)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want exactly the one retry", n)
	}
	tools, err := server.ListTools(context.Background(), rc, agent)
	if err != nil {
		t.Fatalf("list tools on the healed connection: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "pong" {
		t.Fatalf("healed onto the wrong server: %v", toolNames(tools))
	}
}

// killSession makes the request that discovers a restart, on the raw session,
// so the connection dies without any caller of the Server asking for a heal.
func killSession(t *testing.T, session *mcpsdk.ClientSession) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := session.ListTools(context.Background(), nil); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the restart never killed the connection")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuffer collects log lines written from the watcher's goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// waitForLog polls until every want is in the log, or fails.
func waitForLog(t *testing.T, logs *syncBuffer, want ...string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		got := logs.String()
		missing := false
		for _, w := range want {
			if !strings.Contains(got, w) {
				missing = true
			}
		}
		if !missing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log never carried %q; got:\n%s", want, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestThrottledCallerWaitsForTheHeal locks the caller's side of the throttle:
// a tools/list that finds the line dead inside the cooldown waits it out and
// asks again, instead of failing on a death that is being repaired (spec
// §2.16). The log carries the death and the redial, tagged with the server.
func TestThrottledCallerWaitsForTheHeal(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)

	var dials atomic.Int32
	logs := &syncBuffer{}
	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{
			Redial: func(context.Context) (mcpsdk.Transport, error) {
				dials.Add(1)
				return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
			},
			Logger: slog.New(slog.NewTextHandler(logs, nil)),
		})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc := agents.NewRunContext(nil)
	agent := &agents.Agent{Name: "a"}
	if _, err := server.ListTools(context.Background(), rc, agent); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	first := server.session.Load()

	// As if a heal had just landed: the death that follows falls inside the
	// cooldown, so the first redial anyone asks for is throttled.
	forced := time.Now()
	server.dialMu.Lock()
	server.lastDial = forced
	server.cooldown = redialCooldown
	server.dialMu.Unlock()
	swap.set(mcpHandlerWith("pong"))
	killSession(t, first)

	// The caller finds the line dead and waits for the heal instead of failing.
	server.InvalidateToolsCache()
	tools, err := server.ListTools(context.Background(), rc, agent)
	if err != nil {
		t.Fatalf("list tools inside the cooldown: %v", err)
	}
	if since := time.Since(forced); since < redialCooldown {
		t.Errorf("healed %v after the forced dial, inside the %v cooldown", since, redialCooldown)
	}
	if len(tools) != 1 || tools[0].Name != "pong" {
		t.Fatalf("healed onto the wrong server: %v", toolNames(tools))
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want exactly one between the watcher and the caller", n)
	}
	waitForLog(t, logs, "mcp connection lost", "mcp connection redialed", "component=mcp", "mcp=healer")
}

// TestRedialFailureIsLogged locks what the log says when healing itself
// fails: the reason, which no caller's error carries (decisions §5.21).
func TestRedialFailureIsLogged(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)

	logs := &syncBuffer{}
	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{
			Redial: func(context.Context) (mcpsdk.Transport, error) {
				return nil, errors.New("no route to host")
			},
			Logger: slog.New(slog.NewTextHandler(logs, nil)),
		})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	swap.set(mcpHandlerWith("pong"))
	killSession(t, server.session.Load())
	waitForLog(t, logs, "mcp connection lost", "mcp redial failed", "stage=transport", "no route to host")
}

// TestRedialCooldownBacksOff locks the backoff: the cooldown doubles while
// connections die within a minute of being healed, caps at five minutes, and
// returns to the base once a connection lived longer (spec §2.16).
func TestRedialCooldownBacksOff(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)
	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{Redial: func(context.Context) (mcpsdk.Transport, error) {
			return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
		}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// dial redials with the throttle state as given and returns the cooldown
	// the dial decided for the time after it.
	dial := func(cooldown, healedAgo time.Duration) time.Duration {
		t.Helper()
		server.dialMu.Lock()
		server.cooldown = cooldown
		server.lastHealed = time.Now().Add(-healedAgo)
		server.lastDial = time.Time{}
		server.dialMu.Unlock()
		if healed, _ := server.redial(server.session.Load()); !healed {
			t.Fatal("redial did not heal")
		}
		server.dialMu.Lock()
		defer server.dialMu.Unlock()
		return server.cooldown
	}
	if got := dial(redialCooldown, 2*time.Second); got != 2*redialCooldown {
		t.Errorf("cooldown after a heal that died young = %v, want %v", got, 2*redialCooldown)
	}
	if got := dial(4*time.Minute, 2*time.Second); got != redialMaxCooldown {
		t.Errorf("cooldown past the cap = %v, want %v", got, redialMaxCooldown)
	}
	if got := dial(redialMaxCooldown, 2*time.Minute); got != redialCooldown {
		t.Errorf("cooldown after a heal that lived = %v, want the base %v", got, redialCooldown)
	}
}

// TestThrottledCallerGivesUpInBackoff locks the other bound on the caller's
// wait: a line whose cooldown has backed off reports the death at once rather
// than hold a tools/list for minutes.
func TestThrottledCallerGivesUpInBackoff(t *testing.T) {
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	t.Cleanup(endpoint.Close)
	server, err := NewWithTransport(context.Background(), "healer",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL},
		Options{Redial: func(context.Context) (mcpsdk.Transport, error) {
			return &mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL}, nil
		}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc := agents.NewRunContext(nil)
	agent := &agents.Agent{Name: "a"}
	first := server.session.Load()
	server.dialMu.Lock()
	server.lastDial = time.Now()
	server.cooldown = time.Minute
	server.dialMu.Unlock()
	swap.set(mcpHandlerWith("pong"))
	killSession(t, first)

	start := time.Now()
	if _, err := server.ListTools(context.Background(), rc, agent); err == nil {
		t.Fatal("list tools healed inside a minute-long cooldown")
	}
	if waited := time.Since(start); waited > redialCooldown {
		t.Errorf("the caller waited %v on a line in backoff", waited)
	}
}
