package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The surface says where each tool comes from and whether plan mode (and so
// "ask before changes") leaves it alone; the sandbox tools are listed as a
// bound project would add them, read flags from their own constructors.
func TestAgentToolSurfaceCarriesSourceAndReadOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	agents := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{Name: "a", Model: "m", Scope: store.ScopeGlobal, OwnerID: adminUser.ID, Behavior: store.BehaviorGroup{Checklist: true}}
	if err := agents.Create(context.Background(), ac); err != nil {
		t.Fatal(err)
	}
	deps := &bridge.AgentDeps{AgentConfigs: agents, Providers: store.NewProviderStore(db), Sessions: store.NewSessionStore(db),
		Tasks: store.NewTaskStore(db), Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db), Traces: store.NewTraceStore(db)}
	bridge.NewRunner(t.Context(), db, deps)
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	s.RegisterAPI(Handlers{Agents: testAgentConfigHandler(db), Playground: NewPlaygroundHandler(deps)}.Register)

	rec := serve(s.Engine, as(adminUser, http.MethodGet, "/api/v1/agents/"+ac.ID+"/tools", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("tools = %d %s", rec.Code, rec.Body.String())
	}
	var tools []playgroundTool
	if err := json.Unmarshal(rec.Body.Bytes(), &tools); err != nil {
		t.Fatal(err)
	}
	byName := map[string]playgroundTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	for name, want := range map[string]playgroundTool{
		"exec_command": {Source: store.ToolSourceSandbox},
		"apply_patch":  {Source: store.ToolSourceSandbox},
		"read_file":    {Source: store.ToolSourceSandbox, ReadOnly: true},
		"list_files":   {Source: store.ToolSourceSandbox, ReadOnly: true},
		"spawn_task":   {Source: store.ToolSourceTasks},
		"task_status":  {Source: store.ToolSourceTasks, ReadOnly: true},
		"todo_write":   {Source: store.ToolSourceChecklist},
		"submit_plan":  {Source: store.ToolSourcePlan},
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("%s is not listed", name)
			continue
		}
		if got.Source != want.Source || got.ReadOnly != want.ReadOnly {
			t.Errorf("%s: source=%q read_only=%v, want %q/%v", name, got.Source, got.ReadOnly, want.Source, want.ReadOnly)
		}
	}
}

func TestToolSourcesFollowTheBuckets(t *testing.T) {
	got := toolSources([]store.ToolBucket{{Source: "sandbox", Count: 2}, {Source: "tasks", Count: 1}}, 4)
	want := []string{"sandbox", "sandbox", "tasks", ""}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toolSources = %v, want %v", got, want)
		}
	}
}
