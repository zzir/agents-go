package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func TestAgentConfigSecretRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := newTestDB(t)
	st := store.NewAgentConfigStore(db)
	h := NewAgentConfigHandler(st)

	engine := gin.New()
	engine.POST("/agents", h.Create)
	engine.GET("/agents/:id", h.Get)
	engine.PUT("/agents/:id", h.Update)

	// Create with a real key.
	w := doJSON(t, engine, http.MethodPost, "/agents",
		`{"name":"a","model":"gpt-4o","provider":{"api_key":"sk-real-123"},"resilience":{"fallback_models":"[{\"model\":\"m1\",\"api_key\":\"sk-fb-1\"}]"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID       string `json:"id"`
		Provider struct {
			APIKey string `json:"api_key"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.Provider.APIKey != SecretMask {
		t.Errorf("create response api_key = %q, want mask", created.Provider.APIKey)
	}

	// GET must mask both the key and the fallback-model keys.
	w = doJSON(t, engine, http.MethodGet, "/agents/"+created.ID, "")
	body := w.Body.String()
	if strings.Contains(body, "sk-real-123") || strings.Contains(body, "sk-fb-1") {
		t.Fatalf("GET leaked plaintext secret: %s", body)
	}
	if !strings.Contains(body, SecretMask) {
		t.Fatalf("GET should mask the stored key: %s", body)
	}

	// PUT sending the mask back keeps the stored values.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID,
		`{"name":"a2","model":"gpt-4o","provider":{"api_key":"`+SecretMask+`"},"resilience":{"fallback_models":"[{\"model\":\"m1\",\"api_key\":\"`+SecretMask+`\"}]"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d: %s", w.Code, w.Body.String())
	}
	stored, err := st.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.Provider.APIKey != "sk-real-123" {
		t.Errorf("mask round-trip lost api_key: %q", stored.Provider.APIKey)
	}
	if !strings.Contains(stored.Resilience.FallbackModels, "sk-fb-1") {
		t.Errorf("mask round-trip lost fallback key: %q", stored.Resilience.FallbackModels)
	}

	// PUT with a new value replaces, PUT with "" clears.
	doJSON(t, engine, http.MethodPut, "/agents/"+created.ID, `{"name":"a2","model":"gpt-4o","provider":{"api_key":"sk-new"}}`)
	stored, _ = st.Get(ctx, created.ID)
	if stored.Provider.APIKey != "sk-new" {
		t.Errorf("new value not stored: %q", stored.Provider.APIKey)
	}
	doJSON(t, engine, http.MethodPut, "/agents/"+created.ID, `{"name":"a2","model":"gpt-4o","provider":{"api_key":""}}`)
	stored, _ = st.Get(ctx, created.ID)
	if stored.Provider.APIKey != "" {
		t.Errorf("empty value should clear the key: %q", stored.Provider.APIKey)
	}
}

func TestMcpConfigHeaderMasking(t *testing.T) {
	cfg := store.McpServerConfig{
		TransportType: "streamable_http",
		Config:        json.RawMessage(`{"endpoint":"https://x","headers":{"Authorization":"Bearer tok"},"oauth_client_secret":"cs-1"}`),
	}
	masked := sanitizeMcpConfig(cfg)
	s := string(masked.Config)
	if strings.Contains(s, "Bearer tok") || strings.Contains(s, "cs-1") {
		t.Fatalf("sanitize leaked secrets: %s", s)
	}
	if !strings.Contains(s, `"endpoint":"https://x"`) {
		t.Fatalf("sanitize dropped non-secret fields: %s", s)
	}

	// Sending the masked config back restores the stored secrets.
	restored := restoreMcpConfig("streamable_http", masked.Config, cfg.Config)
	rs := string(restored)
	if !strings.Contains(rs, "Bearer tok") || !strings.Contains(rs, "cs-1") {
		t.Fatalf("restore did not resolve masks: %s", rs)
	}

	// A changed header value wins over the stored one; a new header passes through.
	edited := json.RawMessage(`{"endpoint":"https://x","headers":{"Authorization":"Bearer new","X-K":"v"},"oauth_client_secret":"` + SecretMask + `"}`)
	rs = string(restoreMcpConfig("streamable_http", edited, cfg.Config))
	if !strings.Contains(rs, "Bearer new") || !strings.Contains(rs, `"X-K":"v"`) || !strings.Contains(rs, "cs-1") {
		t.Fatalf("partial edit resolved wrong: %s", rs)
	}
}

func TestSandboxPasswordMasking(t *testing.T) {
	cfg := store.SandboxConfig{
		Type:   "ssh",
		Config: json.RawMessage(`{"addr":"h:22","user":"u","password":"pw-1"}`),
	}
	masked := sanitizeSandboxConfig(cfg)
	if strings.Contains(string(masked.Config), "pw-1") {
		t.Fatalf("sanitize leaked password: %s", masked.Config)
	}
	restored := restoreSandboxConfig("ssh", masked.Config, cfg.Config)
	if !strings.Contains(string(restored), "pw-1") {
		t.Fatalf("restore did not resolve password: %s", restored)
	}
}
