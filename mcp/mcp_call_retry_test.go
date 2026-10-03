package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

// A failed dial proves the request never left; nothing else does — not a cut
// after the send, not a deadline, not the transport's "rejected" on its own.
func TestNeverSentIsTheDialFailureAlone(t *testing.T) {
	rejected := &jsonrpc.Error{Code: codeRejected, Message: "rejected by transport"}
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"dial under rejected", fmt.Errorf("sending: %w: %w", rejected, dial), true},
		{"read under rejected", fmt.Errorf("sending: %w: %w", rejected, read), false},
		{"rejected alone", rejected, false},
		{"unexpected eof", io.ErrUnexpectedEOF, false},
		{"deadline", context.DeadlineExceeded, false},
		{"closed", errServerClosed, false},
	} {
		if got := neverSent(tc.err); got != tc.want {
			t.Errorf("%s: neverSent = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The listing's broader rule retries a dial failure too, and still not
	// a rejected request with another cause.
	if !retryable(fmt.Errorf("sending: %w: %w", rejected, dial)) || retryable(fmt.Errorf("sending: %w: %w", rejected, read)) {
		t.Fatal("retryable: a dial failure retries, a cut after the send under rejected does not")
	}
}

// flakyTransport fails the first `fail` tools/call POSTs with err before
// passing requests through; the handshake and the listing are never touched.
type flakyTransport struct {
	next http.RoundTripper
	fail atomic.Int32
	err  error
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.GetBody != nil {
		body, _ := req.GetBody()
		raw, _ := io.ReadAll(body)
		if bytes.Contains(raw, []byte(`"tools/call"`)) && f.fail.Add(-1) >= 0 {
			return nil, f.err
		}
	}
	return f.next.RoundTrip(req)
}

// callThrough connects to a counting MCP server through tr and returns the
// tool, the call count and the server's own HTTP client-side counter.
func callThrough(t *testing.T, tr *flakyTransport, retries int) (*agents.Tool, *atomic.Int32) {
	t.Helper()
	var ran atomic.Int32
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "counter", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "ping", Description: "answer"},
		func(context.Context, *mcpsdk.CallToolRequest, pingArgs) (*mcpsdk.CallToolResult, any, error) {
			ran.Add(1)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}}}, nil, nil
		})
	endpoint := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(endpoint.Close)
	tr.next = http.DefaultTransport
	server, err := NewWithTransport(context.Background(), "counter",
		&mcpsdk.StreamableClientTransport{Endpoint: endpoint.URL, HTTPClient: &http.Client{Transport: tr}},
		Options{MaxRetryAttempts: retries, RetryBackoffBase: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	tools, err := server.ListTools(context.Background(), agents.NewRunContext(nil), &agents.Agent{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if strings.HasSuffix(tool.Name, "ping") {
			return tool, &ran
		}
	}
	t.Fatal("ping not listed")
	return nil, nil
}

// A call whose dial failed is retried and the tool runs once.
func TestCallToolRetriesAFailedDial(t *testing.T) {
	tr := &flakyTransport{err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}
	tr.fail.Store(1)
	tool, ran := callThrough(t, tr, 2)
	res, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{}`)
	if err != nil {
		t.Fatalf("call after a failed dial: %v", err)
	}
	if s, _ := res.ModelOutput().(string); s != "pong" || ran.Load() != 1 {
		t.Fatalf("output = %q, tool ran %d times; want pong once", s, ran.Load())
	}
}

// A call cut after the send is reported, not repeated: the server may have run it.
func TestCallToolDoesNotRetryAFailureAfterTheSend(t *testing.T) {
	tr := &flakyTransport{err: io.ErrUnexpectedEOF}
	tr.fail.Store(1)
	tool, ran := callThrough(t, tr, 2)
	if _, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{}`); err == nil {
		t.Fatal("a call cut after the send was retried into a success")
	}
	if ran.Load() != 0 || tr.fail.Load() != 0 {
		t.Fatalf("tool ran %d times, failures left %d; want 0 and 0", ran.Load(), tr.fail.Load())
	}
}
