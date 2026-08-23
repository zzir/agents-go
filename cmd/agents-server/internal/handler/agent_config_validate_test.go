package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func newAgentEngine(t *testing.T) (*gin.Engine, *store.McpServerStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewAgentConfigHandler(store.NewAgentConfigStore(db), mcpStore, store.NewProviderStore(db), bridge.NewGuardrailResolver(store.NewGuardrailStore(db)))

	engine := newTestEngine()
	engine.POST("/agents", h.Create)
	engine.PUT("/agents/:id", h.Update)
	return engine, mcpStore
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

	s1 := &store.McpServerConfig{Name: "files", TransportType: "stdio"}
	if err := mcpStore.Create(ctx, s1); err != nil {
		t.Fatal(err)
	}

	// Cross-server name collisions are prevented by the unique server name, so
	// the remaining case is the same server selected twice, which would
	// duplicate every one of its tools.
	body := `{"name":"a","model":"gpt-4o","tools":"[\"` + s1.ID + `\",\"` + s1.ID + `\"]"}`
	w := doJSON(t, engine, http.MethodPost, "/agents", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create with doubled id: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "twice") {
		t.Errorf("error should say the server is selected twice: %q", msg)
	}
}

// Distinct MCP server names, unknown ids, and malformed tools JSON must all
// still save — the validator only rejects statically certain collisions.
func TestAgentConfigAcceptsValidToolSelections(t *testing.T) {
	engine, mcpStore := newAgentEngine(t)
	ctx := context.Background()

	s1 := &store.McpServerConfig{Name: "files", TransportType: "stdio"}
	s2 := &store.McpServerConfig{Name: "search", TransportType: "stdio"}
	if err := mcpStore.Create(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := mcpStore.Create(ctx, s2); err != nil {
		t.Fatal(err)
	}

	// Distinct names and unknown ids save (the validator only rejects statically
	// certain collisions); a malformed tools list is now rejected rather than
	// silently dropping every MCP tool at run time.
	for name, body := range map[string]string{
		"distinct names": `{"name":"a","model":"gpt-4o","tools":"[\"` + s1.ID + `\",\"` + s2.ID + `\"]"}`,
		"unknown id":     `{"name":"b","model":"gpt-4o","tools":"[\"` + s1.ID + `\",\"gone\"]"}`,
		"no tools":       `{"name":"d","model":"gpt-4o"}`,
	} {
		if w := doJSON(t, engine, http.MethodPost, "/agents", body); w.Code != http.StatusCreated {
			t.Errorf("%s: got %d, want 201 (body %s)", name, w.Code, w.Body.String())
		}
	}
	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"c","model":"gpt-4o","tools":"not-json"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed tools: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}
