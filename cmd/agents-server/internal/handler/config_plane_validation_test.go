package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// MCP save-time validation rejects an unknown transport and a config missing
// the fields its transport needs, instead of letting a broken server sit in the
// DB until the first connect fails.
func TestMcpServerReqValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     mcpServerReq
		wantSub string // "" = valid
	}{
		{"valid stdio", mcpServerReq{Name: "a", TransportType: "stdio", Config: json.RawMessage(`{"command":"npx"}`)}, ""},
		{"valid http", mcpServerReq{Name: "a", TransportType: "streamable_http", Config: json.RawMessage(`{"endpoint":"http://x"}`)}, ""},
		{"no name", mcpServerReq{TransportType: "stdio", Config: json.RawMessage(`{"command":"x"}`)}, "name is required"},
		{"no transport", mcpServerReq{Name: "a"}, "transport_type is required"},
		{"unknown transport", mcpServerReq{Name: "a", TransportType: "carrier-pigeon"}, "must be stdio or streamable_http"},
		{"stdio no command", mcpServerReq{Name: "a", TransportType: "stdio", Config: json.RawMessage(`{}`)}, "requires config.command"},
		{"http no endpoint", mcpServerReq{Name: "a", TransportType: "streamable_http", Config: json.RawMessage(`{}`)}, "requires config.endpoint"},
		{"stdio bad config", mcpServerReq{Name: "a", TransportType: "stdio", Config: json.RawMessage(`"notobj"`)}, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.req.validate()
			if tc.wantSub == "" && got != "" {
				t.Fatalf("want valid, got %q", got)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Fatalf("want error containing %q, got %q", tc.wantSub, got)
			}
		})
	}
}

// Sandbox save-time validation rejects an unknown type and a docker config that
// fails to parse — the latter must not slip a remote-host config past the block.
func TestSandboxValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	h := NewSandboxHandler(store.NewSandboxStore(db), bridge.NewSandboxManager(t.TempDir()), false)
	engine := gin.New()
	engine.POST("/sandboxes", h.Create)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"valid docker", `{"name":"d","type":"docker","config":{"image":"x"}}`, http.StatusCreated},
		{"unknown type", `{"name":"x","type":"quantum"}`, http.StatusBadRequest},
		{"remote docker host", `{"name":"x","type":"docker","config":{"host":"remote:2375"}}`, http.StatusBadRequest},
		{"malformed docker config bypasses host block", `{"name":"x","type":"docker","config":"notanobject"}`, http.StatusBadRequest},
		{"ssh without addr", `{"name":"x","type":"ssh","config":{"user":"u"}}`, http.StatusBadRequest},
		{"local disabled", `{"name":"x","type":"local"}`, http.StatusForbidden},
		// Field-level strictness (store.NormalizeSandboxConfig): a type
		// mismatch or a missing required field must be refused at save time —
		// stored as-is it would bind sessions to a config that can never
		// build (and, once referenced, could never be repaired).
		{"docker type mismatch", `{"name":"x","type":"docker","config":{"image":"i","persistent":"yes"}}`, http.StatusBadRequest},
		{"docker without image", `{"name":"x","type":"docker","config":{"persistent":true}}`, http.StatusBadRequest},
		{"ssh without user", `{"name":"x","type":"ssh","config":{"addr":"h"}}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := doJSON(t, engine, http.MethodPost, "/sandboxes", tc.body); w.Code != tc.want {
				t.Fatalf("got %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// MCP servers reject a duplicate name (409) — the name is the tool-prefix
// namespace, so two servers can't share it.
func TestMcpServerNameUnique(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	h := NewMcpServerHandler(store.NewMcpServerStore(db), bridge.NewMcpManager(t.Context(), store.NewSettingStore(db)), nil)
	engine := gin.New()
	engine.POST("/mcp-servers", h.Create)

	body := `{"name":"fs","transport_type":"stdio","config":{"command":"npx"}}`
	if w := doJSON(t, engine, http.MethodPost, "/mcp-servers", body); w.Code != http.StatusCreated {
		t.Fatalf("first create: got %d (body %s)", w.Code, w.Body.String())
	}
	if w := doJSON(t, engine, http.MethodPost, "/mcp-servers", body); w.Code != http.StatusConflict {
		t.Fatalf("duplicate name: got %d, want 409 (body %s)", w.Code, w.Body.String())
	}
}

// GET /mcp-servers/:id/tools must distinguish a missing server (404) from one
// that exists but isn't connected (409).
func TestMcpServerToolsNotFoundVsNotConnected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewMcpServerHandler(mcpStore, bridge.NewMcpManager(t.Context(), store.NewSettingStore(db)), nil)
	engine := gin.New()
	engine.GET("/mcp-servers/:id/tools", h.Tools)

	// Missing server -> 404.
	if w := doJSON(t, engine, http.MethodGet, "/mcp-servers/ghost/tools", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing server: got %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	// Exists but never connected -> 409.
	cfg := &store.McpServerConfig{ID: store.NewID(), Name: "fs", TransportType: "stdio"}
	if err := mcpStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, engine, http.MethodGet, "/mcp-servers/"+cfg.ID+"/tools", ""); w.Code != http.StatusConflict {
		t.Fatalf("disconnected server: got %d, want 409 (body %s)", w.Code, w.Body.String())
	}
}

// Provider routes reject a duplicate prefix (409) — a duplicate would make which
// credentials win order-dependent when the router map is built.
func TestProviderRoutePrefixUnique(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	h := NewProviderRouteHandler(store.NewProviderRouteStore(db))
	engine := gin.New()
	engine.POST("/provider-routes", h.Create)

	if w := doJSON(t, engine, http.MethodPost, "/provider-routes", `{"prefix":"gpt","api_key":"k"}`); w.Code != http.StatusCreated {
		t.Fatalf("first create: got %d (body %s)", w.Code, w.Body.String())
	}
	if w := doJSON(t, engine, http.MethodPost, "/provider-routes", `{"prefix":"gpt","api_key":"k2"}`); w.Code != http.StatusConflict {
		t.Fatalf("duplicate prefix: got %d, want 409 (body %s)", w.Code, w.Body.String())
	}
}
