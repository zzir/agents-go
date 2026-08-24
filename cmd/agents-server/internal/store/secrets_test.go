package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/secrets"
)

func withTestBox(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	key[0] = 7
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	UseSecretBox(box)
	t.Cleanup(func() { UseSecretBox(nil) })
}

// rawColumn reads one column of one row straight from SQL — what a database
// dump would show.
func rawColumn(t *testing.T, db *bun.DB, q string, args ...any) string {
	t.Helper()
	var v string
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// With a key: the row holds ciphertext, the store hands back plaintext, and
// the caller's struct is plaintext after every write. Without a key: rows
// written earlier in plaintext still open.
func TestSecretsSealedAtRest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	withTestBox(t)

	providers := NewProviderStore(db)
	pv := &Provider{Name: "p", APIKey: "sk-live"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if pv.APIKey != "sk-live" {
		t.Fatalf("caller's key after Create = %q, want plaintext", pv.APIKey)
	}
	if raw := rawColumn(t, db, "SELECT api_key FROM providers WHERE id = ?", pv.ID); !secrets.IsSealed(raw) {
		t.Fatalf("api_key at rest = %q, want sealed", raw)
	}
	got, err := providers.Get(ctx, pv.ID)
	if err != nil || got.APIKey != "sk-live" {
		t.Fatalf("Get = %+v %v", got, err)
	}
	pv.Name = "p2"
	if err := providers.Update(ctx, pv.ID, pv, nil); err != nil || pv.APIKey != "sk-live" {
		t.Fatalf("Update: %v key=%q", err, pv.APIKey)
	}
	if err := providers.SaveChatGPTToken(ctx, pv.ID, `{"access":"tok"}`); err != nil {
		t.Fatal(err)
	}
	if raw := rawColumn(t, db, "SELECT chatgpt_token FROM providers WHERE id = ?", pv.ID); !secrets.IsSealed(raw) {
		t.Fatalf("chatgpt_token at rest = %q", raw)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.ChatGPTToken != `{"access":"tok"}` {
		t.Fatalf("chatgpt token opened = %q", got.ChatGPTToken)
	}

	// A credential inside a JSON blob: the sandbox SSH password and an MCP
	// server's headers are sealed field by field; the rest stays readable.
	sandboxes := NewSandboxStore(db)
	sb := &SandboxConfig{Name: "ssh", Type: "ssh", Config: json.RawMessage(`{"host":"h","user":"u","password":"hunter2"}`)}
	if err := sandboxes.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	raw := rawColumn(t, db, "SELECT config FROM sandbox_configs WHERE id = ?", sb.ID)
	if strings.Contains(raw, "hunter2") || !strings.Contains(raw, `"host":"h"`) {
		t.Fatalf("sandbox config at rest = %s", raw)
	}
	if got, _ := sandboxes.Get(ctx, sb.ID); !strings.Contains(string(got.Config), `"password":"hunter2"`) {
		t.Fatalf("sandbox config opened = %s", got.Config)
	}
	mcps := NewMcpServerStore(db)
	mc := &McpServerConfig{Name: "m", Config: json.RawMessage(`{"endpoint":"http://x","headers":{"Authorization":"Bearer abc"},"oauth_client_secret":"cs"}`)}
	if err := mcps.Create(ctx, mc); err != nil {
		t.Fatal(err)
	}
	raw = rawColumn(t, db, "SELECT config FROM mcp_servers WHERE id = ?", mc.ID)
	if strings.Contains(raw, "Bearer abc") || strings.Contains(raw, `"cs"`) || !strings.Contains(raw, "http://x") {
		t.Fatalf("mcp config at rest = %s", raw)
	}
	if got, _ := mcps.Get(ctx, mc.ID); !strings.Contains(string(got.Config), "Bearer abc") {
		t.Fatalf("mcp config opened = %s", got.Config)
	}

	// Settings: only the keys the registry calls secret.
	settingsStore := NewSettingStore(db)
	settingsStore.SealIf(func(k string) bool { return k == "openai_api_key" })
	_ = settingsStore.Set(ctx, "openai_api_key", "sk-1")
	_ = settingsStore.Set(ctx, "theme", "dark")
	if raw := rawColumn(t, db, "SELECT value FROM settings WHERE key = ?", "openai_api_key"); !secrets.IsSealed(raw) {
		t.Fatalf("secret setting at rest = %q", raw)
	}
	if raw := rawColumn(t, db, "SELECT value FROM settings WHERE key = ?", "theme"); raw != "dark" {
		t.Fatalf("plain setting at rest = %q", raw)
	}
	if got, _ := settingsStore.Get(ctx, "openai_api_key"); got.Value != "sk-1" {
		t.Fatalf("secret setting opened = %q", got.Value)
	}

	// Rows written before a key was configured (plaintext) still open.
	if _, err := db.NewUpdate().Model((*Provider)(nil)).Set("api_key = ?", "legacy-plain").Where("id = ?", pv.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.APIKey != "legacy-plain" {
		t.Fatalf("legacy plaintext = %q", got.APIKey)
	}
}

// Without a key, a sealed row is an error — never ciphertext handed out as a
// credential.
func TestSealedRowWithoutKeyIsAnError(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	withTestBox(t)
	providers := NewProviderStore(db)
	pv := &Provider{Name: "p", APIKey: "sk-live"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	UseSecretBox(nil)
	if _, err := providers.Get(ctx, pv.ID); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("Get without key = %v, want a no-key error", err)
	}
}

// A sealed value is bound to its column: a provider's ciphertext copied by
// hand into another column — as another provider's key, or as an MCP header
// bound for an attacker's endpoint — does not open there. The same
// ciphertext pasted in through the store is sealed as the text it is.
func TestSealedValueDoesNotRelocate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	withTestBox(t)
	providers := NewProviderStore(db)
	pv := &Provider{Name: "victim", APIKey: "sk-live"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	ct := rawColumn(t, db, "SELECT api_key FROM providers WHERE id = ?", pv.ID)

	mcps := NewMcpServerStore(db)
	mc := &McpServerConfig{Name: "m", Config: json.RawMessage(`{"endpoint":"http://attacker"}`)}
	if err := mcps.Create(ctx, mc); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"endpoint": "http://attacker", "headers": map[string]string{"Authorization": ct}})
	if _, err := db.NewUpdate().Model((*McpServerConfig)(nil)).Set("config = ?", string(cfg)).Where("id = ?", mc.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := mcps.Get(ctx, mc.ID); err == nil {
		t.Fatalf("a provider's ciphertext planted as an MCP header opened: %s", got.Config)
	}

	other := &Provider{Name: "attacker", BaseURL: "http://attacker", APIKey: ct}
	if err := providers.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got, _ := providers.Get(ctx, other.ID); got.APIKey != ct {
		t.Fatalf("pasted ciphertext opened to %q, want the pasted text itself", got.APIKey)
	}
}

// The key is checked at startup against a canary sealed at the first start
// with one: no key where the canary says there was one, or another key,
// fails then — not at the first panel that cannot load.
func TestVerifySecretKeyFailsFastOnAMissingOrChangedKey(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	// No key, no canary: plaintext mode, nothing to check.
	UseSecretBox(nil)
	if err := VerifySecretKey(ctx, db); err != nil {
		t.Fatalf("plaintext mode: %v", err)
	}
	// The first start with a key seals the canary; the next start opens it.
	withTestBox(t)
	if err := VerifySecretKey(ctx, db); err != nil {
		t.Fatalf("first start with a key: %v", err)
	}
	if raw := rawColumn(t, db, "SELECT value FROM settings WHERE key = ?", secretKeyCheck); !secrets.IsSealed(raw) {
		t.Fatalf("canary at rest = %q, want sealed", raw)
	}
	if err := VerifySecretKey(ctx, db); err != nil {
		t.Fatalf("second start with the key: %v", err)
	}
	// The key lost: refused, naming the env var.
	UseSecretBox(nil)
	if err := VerifySecretKey(ctx, db); err == nil || !strings.Contains(err.Error(), "AGENTS_SECRET_KEY") {
		t.Fatalf("no key against a sealed canary = %v, want a refusal naming the key", err)
	}
	// Another key: refused, naming the mismatch.
	other := make([]byte, 32)
	other[0] = 9
	box, _ := secrets.New(other)
	UseSecretBox(box)
	if err := VerifySecretKey(ctx, db); err == nil || !strings.Contains(err.Error(), "sealed under") {
		t.Fatalf("another key against the canary = %v, want a refusal", err)
	}
}
