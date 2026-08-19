package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

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

func mcpHandlerWith(toolName string) http.Handler {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "healer", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: toolName, Description: "answer"},
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}},
			}, nil, nil
		})
	return mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true})
}

// TestManagerConnectionHealsItself proves the wiring, not the mechanism: the
// SDK can heal a dead connection only if it is handed a recipe for rebuilding
// the transport, and the manager is the only place that knows the config it was
// built from. Without that wiring a restarted MCP server stays unreachable for
// every agent until somebody reconnects it by hand.
func TestManagerConnectionHealsItself(t *testing.T) {
	ctx := context.Background()
	swap := &swapHandler{h: mcpHandlerWith("ping")}
	endpoint := httptest.NewServer(swap)
	defer endpoint.Close()

	db := newTestDB(t)
	mgr := NewMcpManager(ctx, settings.NewReader(store.NewSettingStore(db)))
	cfg := &store.McpServerConfig{
		ID: store.NewID(), Name: "healer", TransportType: "streamable_http", Enabled: true,
		Config: []byte(`{"endpoint":"` + endpoint.URL + `"}`),
	}
	if err := mgr.Connect(ctx, cfg); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer mgr.CloseAll()

	srv := mgr.Get(cfg.ID)
	if srv == nil {
		t.Fatal("server not connected")
	}
	rc := agents.NewRunContext(nil)
	if _, err := srv.ListTools(ctx, rc, &agents.Agent{Name: "a"}); err != nil {
		t.Fatalf("list tools: %v", err)
	}

	// The MCP server restarts behind the same address: the session id the
	// client holds is gone, which is what kills the connection.
	swap.set(mcpHandlerWith("pong"))

	deadline := time.Now().Add(15 * time.Second)
	for {
		srv.InvalidateToolsCache()
		tools, err := srv.ListTools(ctx, rc, &agents.Agent{Name: "a"})
		if err == nil {
			if len(tools) != 1 || tools[0].Name != "healer__pong" {
				var names []string
				for _, tool := range tools {
					names = append(names, tool.Name)
				}
				t.Fatalf("healed onto the wrong server: %v", names)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the manager's connection never healed after the server restarted: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
