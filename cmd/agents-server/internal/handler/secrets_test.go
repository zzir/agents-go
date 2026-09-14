package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func TestMcpConfigHeaderMasking(t *testing.T) {
	cfg := store.McpServerConfig{
		Config: json.RawMessage(`{"endpoint":"https://x","headers":{"Authorization":"Bearer tok"},"oauth_client_secret":"cs-1"}`),
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
	restored := restoreMcpConfig(masked.Config, cfg.Config)
	rs := string(restored)
	if !strings.Contains(rs, "Bearer tok") || !strings.Contains(rs, "cs-1") {
		t.Fatalf("restore did not resolve masks: %s", rs)
	}

	// A changed header value wins over the stored one; a new header passes through.
	edited := json.RawMessage(`{"endpoint":"https://x","headers":{"Authorization":"Bearer new","X-K":"v"},"oauth_client_secret":"` + SecretMask + `"}`)
	rs = string(restoreMcpConfig(edited, cfg.Config))
	if !strings.Contains(rs, "Bearer new") || !strings.Contains(rs, `"X-K":"v"`) || !strings.Contains(rs, "cs-1") {
		t.Fatalf("partial edit resolved wrong: %s", rs)
	}
}

func TestSandboxPasswordMasking(t *testing.T) {
	cfg := store.Sandbox{
		Type:   "docker",
		Config: json.RawMessage(`{"host":"ssh://u@h","ssh_password":"pw-1"}`),
	}
	masked := sanitizeSandboxConfig(cfg)
	if strings.Contains(string(masked.Config), "pw-1") {
		t.Fatalf("sanitize leaked password: %s", masked.Config)
	}
	restored := restoreSandboxConfig(masked.Config, cfg.Config)
	if !strings.Contains(string(restored), "pw-1") {
		t.Fatalf("restore did not resolve password: %s", restored)
	}
}

// An e2b sandbox's headers are credentials like its api_key: masked out,
// resolved back on write, and counted as a stored secret by the destination guard.
func TestSandboxHeadersMasking(t *testing.T) {
	cfg := store.Sandbox{
		Type:   "e2b",
		Config: json.RawMessage(`{"api_url":"https://x","api_key":"e2b_1","headers":{"Authorization":"Bearer bl-1"},"template_id":"t"}`),
	}
	masked := sanitizeSandboxConfig(cfg)
	if strings.Contains(string(masked.Config), "bl-1") {
		t.Fatalf("sanitize leaked a header value: %s", masked.Config)
	}
	restored := restoreSandboxConfig(masked.Config, cfg.Config)
	if !strings.Contains(string(restored), "Bearer bl-1") || !strings.Contains(string(restored), "e2b_1") {
		t.Fatalf("restore did not resolve the secrets: %s", restored)
	}
	if !storedSandboxSecret(json.RawMessage(`{"headers":{"Authorization":"Bearer bl-1"}}`)) {
		t.Fatal("a stored header value does not count as a stored secret")
	}
	if storedSandboxSecret(json.RawMessage(`{"headers":{}}`)) {
		t.Fatal("empty headers count as a stored secret")
	}
}

// The credential mask now round-trips on ONE surface: the provider. A masked
// key means "keep the stored one", which only holds while the destination is
// unchanged — moving the backend or the endpoint must refuse it.
func TestProviderUpdateRejectsMaskedKeyAcrossDestinationChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	h := NewProviderHandler(store.NewProviderStore(db))
	engine := newTestEngine()
	engine.POST("/providers", h.Create)
	engine.PUT("/providers/:id", h.Update)

	w := doJSON(t, engine, http.MethodPost, "/providers",
		`{"name":"glm","type":"openai","api_key":"sk-glm-real","base_url":"https://x"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.APIKey != SecretMask {
		t.Fatalf("the key must be masked on the way out, got %q", created.APIKey)
	}

	// Backend change.
	w = doJSON(t, engine, http.MethodPut, "/providers/"+created.ID,
		`{"name":"glm","type":"anthropic","api_key":"********","base_url":"https://x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("masked key across a type switch: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	// Endpoint change.
	w = doJSON(t, engine, http.MethodPut, "/providers/"+created.ID,
		`{"name":"glm","type":"openai","api_key":"********","base_url":"https://y"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("masked key across a base_url change: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	// Same destination: the mask keeps the stored key.
	w = doJSON(t, engine, http.MethodPut, "/providers/"+created.ID,
		`{"name":"glm","type":"openai","api_key":"********","base_url":"https://x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("masked key, same destination: got %d (body %s)", w.Code, w.Body.String())
	}
}
