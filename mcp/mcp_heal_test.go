package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestConnectionHealsAfterTheServerRestarts locks the recovery half.
//
// Nothing in the go-sdk reconnects, so before Redial the FIRST connection
// failure was permanent: every agent configured with that server answered
// "client is closing" until a person noticed and reconnected it by hand. What
// killed it varied — a server restart, a dropped idle socket, somebody else's
// cancelled request — but the outcome never did.
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
// healing is opt-in, because only the caller that owns the configuration knows
// how to rebuild the transport. Without a Redial the old behavior stands, and
// says so.
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

// TestSessionOutlivesItsConnectContext guards the startup path. Auto-connect
// bounds each handshake with a timeout context and cancels it the moment
// Connect returns — and with Redial wired everywhere, a watcher now sits on
// every one of those sessions. If the go-sdk ever ties a session's lifetime
// (or Wait) to the context it was CONNECTED under, that cancel would read as
// the connection dying, and every server would heal-loop from the moment the
// process comes up. This is also what makes bounding redial's own handshake
// safe: redial cancels its connect context the same way.
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
// context it receives is the CONNECTION's, so anything bound to it — the
// subprocess of a stdio server, above all — lives as long as the connection
// does. A context cancelled when redial returns would let a stdio server
// reconnect and be killed in the same breath, and the symptom (healed, then
// immediately dead again) points nowhere near the cause.
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
	server.dialMu.Unlock()

	// The restart kills the connection on the call that discovers it.
	swap.set(mcpHandlerWith("pong"))
	for deadline := time.Now().Add(10 * time.Second); ; {
		server.InvalidateToolsCache()
		if _, err := server.ListTools(context.Background(), rc, agent); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restart never killed the connection")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// No more calls from here: only the watcher can heal the slot.
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
