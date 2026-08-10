package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

type pingArgs struct{}

// parkingTransport holds one request in flight and remembers the context it
// was issued on, which is the whole question here.
type parkingTransport struct {
	inner http.RoundTripper

	mu     sync.Mutex
	armed  bool
	parked context.Context

	release     chan struct{}
	releaseOnce sync.Once
}

// unpark lets the held request go. Idempotent, and the harness always calls it
// on cleanup: a test that fails before releasing would otherwise deadlock in
// Close, which waits for the requests still in flight.
func (p *parkingTransport) unpark() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *parkingTransport) arm() {
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
}

func (p *parkingTransport) parkedCtx() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parked
}

func (p *parkingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	park := p.armed
	if park {
		p.armed = false
		p.parked = r.Context()
	}
	p.mu.Unlock()
	if park {
		// The request's own context is watched too, so a build that still lets
		// a caller's cancellation through unparks here instead of hanging: the
		// point is to observe that context, not to outlast it.
		select {
		case <-p.release:
		case <-r.Context().Done():
		}
	}
	return p.inner.RoundTrip(r)
}

// startSharedServer connects one Server over streamable HTTP — the transport
// whose client connection a cancelled request can kill — through a transport
// that can park one request on demand.
func startSharedServer(t *testing.T) (*Server, *parkingTransport) {
	t.Helper()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "shared", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "ping", Description: "answer at once"},
		func(context.Context, *mcpsdk.CallToolRequest, pingArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}},
			}, nil, nil
		})
	// JSONResponse, because that is what the server this came from answers
	// with: it decides which client path handles the response, and the JSON one
	// is where a failed read takes the connection down with it.
	endpoint := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(endpoint.Close)

	parking := &parkingTransport{inner: http.DefaultTransport, release: make(chan struct{})}
	t.Cleanup(parking.unpark)
	server, err := NewWithTransport(context.Background(), "shared", &mcpsdk.StreamableClientTransport{
		Endpoint:   endpoint.URL,
		HTTPClient: &http.Client{Transport: parking},
	}, Options{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, parking
}

func toolNamed(t *testing.T, server *Server, name string) *agents.Tool {
	t.Helper()
	tools, err := server.ListTools(context.Background(), agents.NewRunContext(nil), &agents.Agent{Name: "a"})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q", name)
	return nil
}

// TestSharedSession_ACallersCancellationDoesNotReachTheRequest locks the
// invariant an afternoon of failed tasks came from.
//
// The session is shared by every agent configured with this server — several
// runs, their background tasks, other conversations — and the streamable HTTP
// transport issues each request on the context it is handed. Let a caller's
// cancellation reach one and the go-sdk fails the whole CONNECTION: the
// response body read returns context.Canceled, handleJSON calls fail(), and
// that is a sync.Once. Every later call by anyone answers "client is closing"
// until something reconnects it, which nothing does. One person stopping one
// run took out five tasks across two conversations that way.
//
// Both halves are the contract: the request survives its caller, and the caller
// still returns at once, so stopping a run stays instant.
func TestSharedSession_ACallersCancellationDoesNotReachTheRequest(t *testing.T) {
	server, parking := startSharedServer(t)
	ping := toolNamed(t, server, "ping")
	rc := agents.NewRunContext(nil)

	// A caller's request is in flight, and the caller is cancelled right there
	// — a person pressing Stop on a run.
	parking.arm()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ping.OnInvoke(ctx, &agents.ToolContext{RunContext: rc}, `{}`)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for parking.parkedCtx() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no request was ever issued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	// The caller returns at once, and says it was cancelled.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the cancelled caller reported %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled caller did not return")
	}

	// And the request it left behind is untouched. Give a cancellation that IS
	// travelling the time to arrive before concluding that it is not.
	time.Sleep(200 * time.Millisecond)
	if err := parking.parkedCtx().Err(); err != nil {
		t.Errorf("the caller's cancellation reached its request on the shared session (%v) — "+
			"the response read fails, the go-sdk fails the whole connection with it, and every "+
			"other run on this server gets \"client is closing\" until it is reconnected", err)
	}
	parking.unpark()

	// The connection still answers everyone else. Both halves matter: listing
	// is what a run does at the top of every turn, calling is what it does
	// after — the failure this guards took out both.
	if _, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "b"}); err != nil {
		t.Errorf("listing tools after another caller's cancellation: %v", err)
	}
	out, err := ping.OnInvoke(context.Background(), &agents.ToolContext{RunContext: rc}, `{}`)
	if err != nil {
		t.Fatalf("calling a tool after another caller's cancellation: %v", err)
	}
	if got := out.Text(); got != "pong" {
		t.Errorf("tool answered %q, want pong", got)
	}
}
