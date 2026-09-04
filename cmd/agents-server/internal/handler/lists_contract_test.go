package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// Every list endpoint answers [] for an empty collection, never null — the
// frontend maps over the result directly (protocol.md, Response conventions).
func TestListsNeverNull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	engine := newTestEngine()

	// listVisible-backed lists and the hand-written ones both flow through
	// nonNilList; a session id with no traces exercises the trace path.
	engine.GET("/agents", testAgentConfigHandler(db).List)
	engine.GET("/providers", NewProviderHandler(store.NewProviderStore(db)).List)
	engine.GET("/workflows", NewWorkflowHandler(store.NewWorkflowStore(db), store.NewAgentConfigStore(db), store.NewSessionStore(db), nil).List)
	engine.GET("/mcp-servers", NewMcpServerHandler(store.NewMcpServerStore(db), nil, nil, "").List)
	engine.GET("/memories", NewMemoryHandler(store.NewMemoryStore(db)).List)
	engine.GET("/skills", NewSkillHandler(store.NewSkillStore(db), settings.NewReader(nil)).List)
	engine.GET("/sessions/:id/traces", NewTraceHandler(store.NewTraceStore(db)).ListBySession)
	runner := bridge.NewRunner(t.Context(), db, &bridge.AgentDeps{AgentConfigs: store.NewAgentConfigStore(db), Sessions: store.NewSessionStore(db), Traces: store.NewTraceStore(db)})
	engine.GET("/sessions/:id/tasks", NewTaskHandler(store.NewTaskStore(db), runner).ListBySession)

	// Guardrails is omitted: its resolver always returns the built-in defs, so
	// the collection is never empty to begin with.
	paths := []string{"/agents", "/providers", "/workflows", "/mcp-servers", "/memories", "/skills", "/sessions/none/traces", "/sessions/none/tasks"}
	for _, p := range paths {
		w := doJSON(t, engine, http.MethodGet, p, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d (%s)", p, w.Code, w.Body.String())
			continue
		}
		if got := strings.TrimSpace(w.Body.String()); got != "[]" {
			t.Errorf("GET %s empty: got %q, want []", p, got)
		}
	}
}

// PUT is a full replace: a field omitted from the body is cleared, not kept.
// The masked-secret exception aside, a partial update wipes what it omits —
// pinned by protocol.md so the contract cannot drift unnoticed.
func TestUpdateIsFullReplace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	providers := store.NewProviderStore(db)
	if err := providers.Create(ctx, &store.Provider{ID: store.NewID(), Name: "p", Type: "openai", OwnerID: store.LocalUserID}); err != nil {
		t.Fatal(err)
	}
	var prov store.Provider
	rows, err := providers.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("provider list: %v (%d)", err, len(rows))
	}
	prov = rows[0]

	ah := testAgentConfigHandler(db)
	engine := newTestEngine()
	engine.POST("/agents", ah.Create)
	engine.PUT("/agents/:id", ah.Update)
	engine.GET("/agents/:id", ah.Get)

	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","model":"m","provider_id":"`+prov.ID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create agent: %d (%s)", w.Code, w.Body.String())
	}
	var created struct {
		ID         string `json:"id"`
		ProviderID string `json:"provider_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ProviderID != prov.ID {
		t.Fatalf("created provider_id = %q, want %q", created.ProviderID, prov.ID)
	}

	// PUT without provider_id: a full replace clears it.
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID, `{"name":"a","model":"m2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update agent: %d (%s)", w.Code, w.Body.String())
	}
	w = doJSON(t, engine, http.MethodGet, "/agents/"+created.ID, "")
	var got struct {
		ProviderID string `json:"provider_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ProviderID != "" {
		t.Fatalf("after partial PUT, provider_id = %q, want cleared", got.ProviderID)
	}
}
