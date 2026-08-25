package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// MCP save-time validation rejects a config that cannot connect — a missing
// or non-URL endpoint, an unknown auth mode, out-of-range retry settings —
// instead of letting a broken server sit in the DB until the first connect fails.
func TestMcpServerReqValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     mcpServerReq
		wantSub string // "" = valid
	}{
		{"valid http", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http://x"}`)}, ""},
		{"valid https oauth", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"https://x/mcp","auth_mode":"oauth"}`)}, ""},
		{"valid header mode", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http://x","auth_mode":"header"}`)}, ""},
		{"valid unlimited retries", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http://x","max_retry_attempts":-1,"retry_backoff_ms":500}`)}, ""},
		{"no name", mcpServerReq{Config: json.RawMessage(`{"endpoint":"http://x"}`)}, "name is required"},
		{"no config", mcpServerReq{Name: "a"}, "config.endpoint is required"},
		{"no endpoint", mcpServerReq{Name: "a", Config: json.RawMessage(`{}`)}, "config.endpoint is required"},
		{"bad config", mcpServerReq{Name: "a", Config: json.RawMessage(`"notobj"`)}, "not valid JSON"},
		{"not a url", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"not-a-url"}`)}, "absolute http(s) URL"},
		{"wrong scheme", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"ftp://x/mcp"}`)}, "absolute http(s) URL"},
		{"no host", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http:///mcp"}`)}, "absolute http(s) URL"},
		{"unknown auth mode", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http://x","auth_mode":"basic"}`)}, "auth_mode"},
		{"retries below -1", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http://x","max_retry_attempts":-2}`)}, "max_retry_attempts"},
		{"negative backoff", mcpServerReq{Name: "a", Config: json.RawMessage(`{"endpoint":"http://x","retry_backoff_ms":-1}`)}, "retry_backoff_ms"},
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
	db := testdb.New(t)
	h := testSandboxHandler(store.NewSandboxStore(db), sandboxes.NewManager(t.TempDir()), t.TempDir())
	engine := newTestEngine()
	engine.POST("/sandboxes", h.Create)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"valid docker", `{"name":"d","type":"docker","config":{"image":"x"}}`, http.StatusCreated},
		{"valid remote docker", `{"name":"r","type":"docker","config":{"image":"x","host":"ssh://u@h"}}`, http.StatusCreated},
		{"unknown type", `{"name":"x","type":"quantum"}`, http.StatusBadRequest},
		{"retired local type", `{"name":"x","type":"local"}`, http.StatusBadRequest},
		{"retired ssh type", `{"name":"x","type":"ssh","config":{"addr":"h","user":"u"}}`, http.StatusBadRequest},
		{"bare host refused", `{"name":"x","type":"docker","config":{"image":"x","host":"remote:2375"}}`, http.StatusBadRequest},
		{"ssh host without user refused", `{"name":"x","type":"docker","config":{"image":"x","host":"ssh://h"}}`, http.StatusBadRequest},
		{"malformed docker config", `{"name":"x","type":"docker","config":"notanobject"}`, http.StatusBadRequest},
		// Field-level strictness (store.NormalizeSandboxConfig): a type
		// mismatch or a missing required field must be refused at save time —
		// stored as-is it would bind sessions to a config that can never
		// build (and, once referenced, could never be repaired).
		{"docker type mismatch", `{"name":"x","type":"docker","config":{"image":"i","network":"yes"}}`, http.StatusBadRequest},
		{"docker without image", `{"name":"x","type":"docker","config":{"persistent":true}}`, http.StatusBadRequest},
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
	db := testdb.New(t)
	h := NewMcpServerHandler(store.NewMcpServerStore(db), mcpservers.NewManager(t.Context(), settings.NewReader(store.NewSettingStore(db))), nil, "")
	engine := newTestEngine()
	engine.POST("/mcp-servers", h.Create)

	body := `{"name":"fs","config":{"endpoint":"http://x"}}`
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
	db := testdb.New(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewMcpServerHandler(mcpStore, mcpservers.NewManager(t.Context(), settings.NewReader(store.NewSettingStore(db))), nil, "")
	engine := newTestEngine()
	engine.GET("/mcp-servers/:id/tools", h.Tools)

	// Missing server -> 404.
	if w := doJSON(t, engine, http.MethodGet, "/mcp-servers/ghost/tools", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing server: got %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	// Exists but never connected -> 409.
	cfg := &store.McpServerConfig{ID: store.NewID(), Name: "fs", Config: json.RawMessage(`{"endpoint":"http://x"}`)}
	if err := mcpStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, engine, http.MethodGet, "/mcp-servers/"+cfg.ID+"/tools", ""); w.Code != http.StatusConflict {
		t.Fatalf("disconnected server: got %d, want 409 (body %s)", w.Code, w.Body.String())
	}
}
