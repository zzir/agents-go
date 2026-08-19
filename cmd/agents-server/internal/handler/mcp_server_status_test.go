package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// status must collapse config + live state into the one value the UI renders:
// disabled wins over everything, an OAuth server without a token needs
// authorization, and one WITH a token is merely disconnected (a plain Connect
// reconnects it silently — no popup, so no needs_auth).
func TestMcpServerStatusDerivation(t *testing.T) {
	db := newTestDB(t)
	h := NewMcpServerHandler(store.NewMcpServerStore(db), bridge.NewMcpManager(t.Context(), settings.NewReader(store.NewSettingStore(db))), nil)

	oauthCfg := json.RawMessage(`{"endpoint":"http://x","auth_mode":"oauth"}`)
	cases := []struct {
		name string
		cfg  store.McpServerConfig
		want string
	}{
		{"disabled stdio", store.McpServerConfig{ID: "a", TransportType: "stdio", Enabled: false}, "disabled"},
		{"disabled oauth without token", store.McpServerConfig{ID: "b", TransportType: "streamable_http", Config: oauthCfg, Enabled: false}, "disabled"},
		{"enabled stdio not connected", store.McpServerConfig{ID: "c", TransportType: "stdio", Enabled: true}, "disconnected"},
		{"oauth without token", store.McpServerConfig{ID: "d", TransportType: "streamable_http", Config: oauthCfg, Enabled: true}, "needs_auth"},
		{"oauth with saved token", store.McpServerConfig{ID: "e", TransportType: "streamable_http", Config: oauthCfg, OAuthToken: `{"access_token":"t"}`, Enabled: true}, "disconnected"},
		{"http without oauth", store.McpServerConfig{ID: "f", TransportType: "streamable_http", Config: json.RawMessage(`{"endpoint":"http://x"}`), Enabled: true}, "disconnected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.status(&tc.cfg); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// Connecting a disabled server must be refused: agents pick tools by live
// connection state, so a connection here would put the server's tools back in
// play and silently void the disable switch.
func TestMcpServerConnectRejectsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewMcpServerHandler(mcpStore, bridge.NewMcpManager(t.Context(), settings.NewReader(store.NewSettingStore(db))), nil)
	engine := gin.New()
	engine.POST("/mcp-servers/:id/connect", h.Connect)

	cfg := &store.McpServerConfig{ID: store.NewID(), Name: "fs", TransportType: "stdio", Config: json.RawMessage(`{"command":"true"}`), Enabled: false}
	if err := mcpStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, engine, http.MethodPost, "/mcp-servers/"+cfg.ID+"/connect", ""); w.Code != http.StatusConflict {
		t.Fatalf("connect on disabled server: got %d, want 409 (body %s)", w.Code, w.Body.String())
	}
}

// Disabling a server via PUT must report status "disabled" — not leave the row
// looking like a disconnected server inviting a reconnect (the old UI showed a
// Connect button right after disabling).
func TestMcpServerUpdateDisableReportsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewMcpServerHandler(mcpStore, bridge.NewMcpManager(t.Context(), settings.NewReader(store.NewSettingStore(db))), nil)
	engine := gin.New()
	engine.PUT("/mcp-servers/:id", h.Update)

	cfg := &store.McpServerConfig{ID: store.NewID(), Name: "fs", TransportType: "stdio", Config: json.RawMessage(`{"command":"true"}`), Enabled: true}
	if err := mcpStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"fs","transport_type":"stdio","enabled":false,"config":{"command":"true"}}`
	w := doJSON(t, engine, http.MethodPut, "/mcp-servers/"+cfg.ID, body)
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "disabled" {
		t.Fatalf("status after disable = %q, want %q", resp.Status, "disabled")
	}
}
