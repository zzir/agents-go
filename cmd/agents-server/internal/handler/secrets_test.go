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

// A masked key round-trips only within the backend it was stored for:
// switching provider_type while keeping the ******** mask must be rejected,
// or the old provider's real credential would be sent to the new provider.
func TestAgentUpdateRejectsMaskedKeyAcrossProviderSwitch(t *testing.T) {
	engine, _ := newAgentEngine(t)

	w := doJSON(t, engine, http.MethodPost, "/agents",
		`{"name":"a","model":"gpt-4o","provider":{"provider_type":"openai","api_key":"sk-openai-real"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// Provider switched, mask kept: refused.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID,
		`{"name":"a","model":"claude-opus-5","provider":{"provider_type":"anthropic","api_key":"********"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("masked key across provider switch: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "provider_type or base_url changed") {
		t.Errorf("error should explain the destination change: %q", msg)
	}

	// A replacement key or an explicit clear both pass.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID,
		`{"name":"a","model":"claude-opus-5","provider":{"provider_type":"anthropic","api_key":"sk-ant-new"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("replacement key: got %d (body %s)", w.Code, w.Body.String())
	}
	// Same provider (spelled explicitly vs the "" default): the mask is fine.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID,
		`{"name":"a","model":"claude-opus-5","provider":{"provider_type":"anthropic","api_key":"********"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("masked key, same provider: got %d (body %s)", w.Code, w.Body.String())
	}
}

// The same rule for provider routes.
func TestProviderRouteUpdateRejectsMaskedKeyAcrossProviderSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	h := NewProviderRouteHandler(store.NewProviderRouteStore(db))
	engine := gin.New()
	engine.POST("/provider-routes", h.Create)
	engine.PUT("/provider-routes/:id", h.Update)

	w := doJSON(t, engine, http.MethodPost, "/provider-routes",
		`{"prefix":"glm","provider_type":"openai","api_key":"sk-glm-real","base_url":"https://x"}`)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	w = doJSON(t, engine, http.MethodPut, "/provider-routes/"+created.ID,
		`{"prefix":"glm","provider_type":"anthropic","api_key":"********","base_url":"https://x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("masked key across provider switch: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	w = doJSON(t, engine, http.MethodPut, "/provider-routes/"+created.ID,
		`{"prefix":"glm","provider_type":"anthropic","api_key":"sk-ant-new","base_url":"https://x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("replacement key: got %d (body %s)", w.Code, w.Body.String())
	}
}

// Fallback keys restore strictly by (normalized provider_type, model): never
// across providers, never by position.
func TestRestoreFallbackModelsMatchesByProviderAndModel(t *testing.T) {
	prev := `[{"model":"m1","provider_type":"","api_key":"k-openai"},{"model":"m2","provider_type":"anthropic","api_key":"k-ant"}]`

	// Reordered entries still restore by identity ("" normalizes to openai).
	got := restoreFallbackModels(
		`[{"model":"m2","provider_type":"anthropic","api_key":"********"},{"model":"m1","provider_type":"openai","api_key":"********"}]`, prev)
	if !strings.Contains(got, `"k-ant"`) || !strings.Contains(got, `"k-openai"`) || strings.Contains(got, SecretMask) {
		t.Fatalf("identity restore failed: %s", got)
	}

	// Same model, switched provider: no restore — the mask clears instead of
	// carrying the old provider's key across.
	got = restoreFallbackModels(`[{"model":"m1","provider_type":"anthropic","api_key":"********"}]`, prev)
	if strings.Contains(got, "k-openai") {
		t.Fatalf("cross-provider restore must not happen: %s", got)
	}
	if !strings.Contains(got, `"api_key":""`) {
		t.Fatalf("unmatched mask must clear: %s", got)
	}

	// A new model at the position of an old entry gets nothing (no positional
	// fallback).
	got = restoreFallbackModels(`[{"model":"m9","provider_type":"openai","api_key":"********"}]`, prev)
	if strings.Contains(got, "k-openai") || strings.Contains(got, "k-ant") {
		t.Fatalf("positional restore must not happen: %s", got)
	}
}

// The credential's identity includes the ENDPOINT: same provider_type but a
// different base_url is a different destination, and a masked key must not
// follow it there. Trailing-slash variants of the same endpoint are not a
// change.
func TestMaskedKeyRejectedAcrossBaseURLChange(t *testing.T) {
	engine, _ := newAgentEngine(t)

	w := doJSON(t, engine, http.MethodPost, "/agents",
		`{"name":"a","model":"m","provider":{"provider_type":"openai","api_key":"sk-real","base_url":"https://one.example/v1"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// Endpoint switched, mask kept: refused.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID,
		`{"name":"a","model":"m","provider":{"provider_type":"openai","api_key":"********","base_url":"https://two.example/v1"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("masked key across base_url change: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	// A trailing slash is the same endpoint.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID,
		`{"name":"a","model":"m","provider":{"provider_type":"openai","api_key":"********","base_url":"https://one.example/v1/"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("slash variant of the same endpoint: got %d (body %s)", w.Code, w.Body.String())
	}
}

// Fallback identity includes base_url too: two same-provider entries pointed
// at different OpenAI-compatible endpoints hold different credentials.
func TestRestoreFallbackModelsRespectsBaseURL(t *testing.T) {
	prev := `[{"model":"m","provider_type":"openai","base_url":"https://one.example/v1","api_key":"k-one"},` +
		`{"model":"m","provider_type":"openai","base_url":"https://two.example/v1","api_key":"k-two"}]`

	// Each entry restores its own endpoint's key.
	got := restoreFallbackModels(
		`[{"model":"m","provider_type":"openai","base_url":"https://two.example/v1","api_key":"********"},`+
			`{"model":"m","provider_type":"openai","base_url":"https://one.example/v1","api_key":"********"}]`, prev)
	if !strings.Contains(got, `"k-one"`) || !strings.Contains(got, `"k-two"`) || strings.Contains(got, SecretMask) {
		t.Fatalf("per-endpoint restore failed: %s", got)
	}

	// A moved endpoint gets nothing — the mask clears.
	got = restoreFallbackModels(
		`[{"model":"m","provider_type":"openai","base_url":"https://three.example/v1","api_key":"********"}]`, prev)
	if strings.Contains(got, "k-one") || strings.Contains(got, "k-two") {
		t.Fatalf("cross-endpoint restore must not happen: %s", got)
	}

	// Slash variants are the same endpoint.
	got = restoreFallbackModels(
		`[{"model":"m","provider_type":"openai","base_url":"https://one.example/v1/","api_key":"********"}]`, prev)
	if !strings.Contains(got, `"k-one"`) {
		t.Fatalf("slash variant must restore: %s", got)
	}
}

// Two same-identity fallback entries (key rotation against one endpoint)
// each keep their OWN key across a masked round-trip: keys queue per
// identity and are consumed in order, never collapsed onto the first.
func TestRestoreFallbackModelsKeyRotationQueue(t *testing.T) {
	prev := `[{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"k-first"},` +
		`{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"k-second"}]`

	got := restoreFallbackModels(
		`[{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"********"},`+
			`{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"********"}]`, prev)
	first := strings.Index(got, "k-first")
	second := strings.Index(got, "k-second")
	if first < 0 || second < 0 {
		t.Fatalf("both rotation keys must survive: %s", got)
	}
	if first > second {
		t.Fatalf("queue order must follow stored order: %s", got)
	}

	// A third masked entry of the same identity has no key left: it clears.
	got = restoreFallbackModels(
		`[{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"********"},`+
			`{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"********"},`+
			`{"model":"m","provider_type":"openai","base_url":"https://one.example","api_key":"********"}]`, prev)
	if strings.Count(got, "k-first") != 1 || strings.Count(got, "k-second") != 1 || !strings.Contains(got, `"api_key":""`) {
		t.Fatalf("exhausted queue must clear the extra mask: %s", got)
	}
}
