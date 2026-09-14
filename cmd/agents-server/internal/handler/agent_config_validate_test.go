package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func newAgentEngine(t *testing.T) (*gin.Engine, *store.McpServerStore) {
	t.Helper()
	engine, mcpStore, _ := newAgentEngineDB(t)
	return engine, mcpStore
}

// newAgentEngineDB also hands back the database, for rows a test seeds directly.
func newAgentEngineDB(t *testing.T) (*gin.Engine, *store.McpServerStore, *bun.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewAgentConfigHandler(store.NewAgentConfigStore(db), mcpStore, store.NewProviderStore(db), store.NewSkillStore(db), guardrails.NewResolver(store.NewGuardrailStore(db)))

	engine := newTestEngine()
	engine.POST("/agents", h.Create)
	engine.GET("/agents/:id", h.Get)
	engine.PUT("/agents/:id", h.Update)
	return engine, mcpStore, db
}

func errMessage(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("response is not the error envelope: %s", body)
	}
	if envelope.Error.Code != protocol.CodeValidation {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, protocol.CodeValidation)
	}
	return envelope.Error.Message
}

// The OpenAI provider ships no default model, so a config with no model would
// only fail at run time — it is rejected at save time instead, on create and
// on update.
func TestAgentConfigRejectsEmptyModel(t *testing.T) {
	engine, _ := newAgentEngine(t)

	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "model") {
		t.Errorf("error should name the model field: %q", msg)
	}

	// A valid create, then an update clearing the model: also rejected.
	w = doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","model":"gpt-4o"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID, `{"name":"a","model":""}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("update clearing model: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// Only the built-in catalog is a valid avatar: an external URL would be
// blocked by the CSP and render as a broken image, so it is refused at save.
func TestAgentConfigAvatarShape(t *testing.T) {
	engine, _ := newAgentEngine(t)

	for _, bad := range []string{"https://evil.example/x.svg", "/avatars/../secret.svg", "/avatars/x.png", "avatars/X.svg"} {
		w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","model":"gpt-4o","avatar":"`+bad+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("avatar %q: got %d, want 400 (body %s)", bad, w.Code, w.Body.String())
			continue
		}
		if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "avatar") {
			t.Errorf("error should name the avatar field: %q", msg)
		}
	}

	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","model":"gpt-4o","avatar":"/avatars/Architect.svg"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("built-in avatar: got %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	w = doJSON(t, engine, http.MethodPost, "/agents", `{"name":"b","model":"gpt-4o"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("no avatar: got %d, want 201 (body %s)", w.Code, w.Body.String())
	}
}

// use_previous_response_id was removed end to end; a client still sending the
// stale key is simply ignored (unknown JSON field), not rejected — legacy
// callers keep working.
func TestAgentConfigIgnoresLegacyUsePreviousResponseID(t *testing.T) {
	engine, _ := newAgentEngine(t)

	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","model":"gpt-4o","session":{"use_previous_response_id":true,"history_limit":3}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with stale key: got %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	var created struct {
		Session struct {
			HistoryLimit int `json:"history_limit"`
		} `json:"session"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.Session.HistoryLimit != 3 {
		t.Errorf("history_limit = %d, want 3 — the rest of the session group must still bind", created.Session.HistoryLimit)
	}
}

// Two selected MCP servers sharing a name would prefix all their tools
// identically ("<name>__"), a guaranteed run-time collision — rejected at
// save time with the offending name in the message.
func TestAgentConfigRejectsDoubleSelectedMcpServer(t *testing.T) {
	engine, mcpStore := newAgentEngine(t)
	ctx := context.Background()

	s1 := &store.McpServerConfig{Name: "files", OwnerID: store.LocalUserID}
	if err := mcpStore.Create(ctx, s1); err != nil {
		t.Fatal(err)
	}

	// Cross-server name collisions are prevented by the unique server name, so
	// the remaining case is the same server selected twice, which would
	// duplicate every one of its tools.
	body := `{"name":"a","model":"gpt-4o","tools":["` + s1.ID + `","` + s1.ID + `"]}`
	w := doJSON(t, engine, http.MethodPost, "/agents", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create with doubled id: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "twice") {
		t.Errorf("error should say the server is selected twice: %q", msg)
	}
}

// Distinct MCP server names and unknown ids save — the validator only rejects
// statically certain collisions; a string where the array goes is refused at bind.
func TestAgentConfigAcceptsValidToolSelections(t *testing.T) {
	engine, mcpStore := newAgentEngine(t)
	ctx := context.Background()

	s1 := &store.McpServerConfig{Name: "files", OwnerID: store.LocalUserID}
	s2 := &store.McpServerConfig{Name: "search", OwnerID: store.LocalUserID}
	if err := mcpStore.Create(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := mcpStore.Create(ctx, s2); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"distinct names": `{"name":"a","model":"gpt-4o","tools":["` + s1.ID + `","` + s2.ID + `"]}`,
		"unknown id":     `{"name":"b","model":"gpt-4o","tools":["` + s1.ID + `","gone"]}`,
		"no tools":       `{"name":"d","model":"gpt-4o"}`,
	} {
		if w := doJSON(t, engine, http.MethodPost, "/agents", body); w.Code != http.StatusCreated {
			t.Errorf("%s: got %d, want 201 (body %s)", name, w.Code, w.Body.String())
		}
	}
	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"c","model":"gpt-4o","tools":"[\"`+s1.ID+`\"]"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("tools as a JSON string: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// The list fields are arrays on the wire, and they come back as arrays: an
// absent skills selection reads null, an explicit [] stays [].
func TestAgentConfigListFieldsRoundTripAsArrays(t *testing.T) {
	engine, _ := newAgentEngine(t)

	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","model":"gpt-4o","approval":{"approve_tools":["exec_command"]},"tools":["m1"],"handoffs":["h1"],"skills":[]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	type lists struct {
		ID       string    `json:"id"`
		Tools    []string  `json:"tools"`
		Handoffs []string  `json:"handoffs"`
		Skills   *[]string `json:"skills"`
		Approval struct {
			ApproveTools []string `json:"approve_tools"`
		} `json:"approval"`
	}
	var got lists
	decode := func() {
		t.Helper()
		got = lists{}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
	}
	decode()
	if len(got.Approval.ApproveTools) != 1 || got.Approval.ApproveTools[0] != "exec_command" || len(got.Tools) != 1 || len(got.Handoffs) != 1 {
		t.Fatalf("lists did not round-trip: %s", w.Body.String())
	}
	if got.Skills == nil || len(*got.Skills) != 0 {
		t.Fatalf("an explicit empty skills selection must read back as [], got %s", w.Body.String())
	}

	w = doJSON(t, engine, http.MethodPut, "/agents/"+got.ID, `{"name":"a","model":"gpt-4o"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	decode()
	if got.Skills != nil {
		t.Fatalf("an absent skills selection must read back as null, got %s", w.Body.String())
	}
	if len(got.Tools) != 0 || len(got.Approval.ApproveTools) != 0 {
		t.Fatalf("an omitted list must clear, got %s", w.Body.String())
	}
}

// A name past the cap is refused at bind.
func TestAgentConfigRejectsLongName(t *testing.T) {
	engine, _ := newAgentEngine(t)
	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"`+strings.Repeat("n", maxNameLen+1)+`","model":"m"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(errMessage(t, w.Body.Bytes()), "longer than") {
		t.Fatalf("create with a long name = %d %s, want 400", w.Code, w.Body.String())
	}
}

// A fallback entry names a provider by id and nothing else: a key in an entry
// is refused (it lives on the provider), the endpoint fields are read-only,
// and the provider must exist and be one the agent may reference. A row from
// before provider_id reads back with its endpoint and never its key.
func TestAgentConfigFallbackEntriesNameProviders(t *testing.T) {
	engine, _, db := newAgentEngineDB(t)
	ctx := context.Background()
	providers := store.NewProviderStore(db)
	pv := &store.Provider{OwnerID: store.LocalUserID, Name: "fb", Type: "anthropic", APIKey: "sk-a"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ body, want string }{
		"inline key":     {`{"name":"k","model":"m","resilience":{"fallback_models":[{"provider_id":"` + pv.ID + `","api_key":"sk-x"}]}}`, "api_key"},
		"no provider":    {`{"name":"n","model":"m","resilience":{"fallback_models":[{"model":"m2"}]}}`, "provider_id is required"},
		"endpoint form":  {`{"name":"e","model":"m","resilience":{"fallback_models":[{"provider_id":"` + pv.ID + `","base_url":"https://x"}]}}`, "read-only"},
		"unknown id":     {`{"name":"u","model":"m","resilience":{"fallback_models":[{"provider_id":"` + store.NewID() + `"}]}}`, "names no provider"},
		"misspelled key": {`{"name":"s","model":"m","resilience":{"fallback_models":[{"providerId":"` + pv.ID + `"}]}}`, "unknown field"},
	} {
		w := doJSON(t, engine, http.MethodPost, "/agents", tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: got %d %s, want 400 mentioning %q", name, w.Code, w.Body.String(), tc.want)
		}
	}
	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"ok","model":"m","resilience":{"fallback_models":[{"provider_id":"`+pv.ID+`","model":"claude"}]}}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"provider_id":"`+pv.ID+`"`) {
		t.Fatalf("a provider the agent may reference: got %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// A row as an earlier build wrote it: the group holds the chain as a JSON
	// string, each entry with its own key.
	legacy := `{"fallback_models":"[{\"model\":\"m\",\"provider_type\":\"anthropic\",\"base_url\":\"https://a.example\",\"api_key\":\"sk-old\"}]"}`
	if _, err := db.NewUpdate().Model((*store.AgentConfig)(nil)).Set("resilience = ?", legacy).Where("id = ?", created.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	w = doJSON(t, engine, http.MethodGet, "/agents/"+created.ID, "")
	body := w.Body.String()
	if w.Code != http.StatusOK || strings.Contains(body, "sk-old") || strings.Contains(body, "api_key") {
		t.Fatalf("a legacy key must never reach a client: %d %s", w.Code, body)
	}
	if !strings.Contains(body, `"provider_type":"anthropic"`) || !strings.Contains(body, `"base_url":"https://a.example"`) {
		t.Fatalf("a legacy entry reads back with its endpoint: %s", body)
	}
}
