package bridge

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// testDeps is a complete dependency set over db, as the composition root
// builds one. bridgetest carries the copy other packages' tests import.
func testDeps(tb testing.TB, db *bun.DB) *AgentDeps {
	tb.Helper()
	mcp, sandboxMgr := mcpservers.NewManager(context.Background()), sandboxes.NewManager()
	tb.Cleanup(func() {
		mcp.CloseAll()
		sandboxMgr.CloseAll()
	})
	providerStore := store.NewProviderStore(db)
	return &AgentDeps{
		AgentConfigs:     store.NewAgentConfigStore(db),
		Providers:        providerStore,
		McpServers:       store.NewMcpServerStore(db),
		Sandboxes:        store.NewSandboxStore(db),
		Skills:           store.NewSkillStore(db),
		Workspaces:       store.NewWorkspaceStore(db),
		Memories:         store.NewMemoryStore(db),
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Sessions:         store.NewSessionStore(db),
		Traces:           store.NewTraceStore(db),
		Guardrails:       guardrails.NewResolver(store.NewGuardrailStore(db)),
		McpManager:       mcp,
		SandboxManager:   sandboxMgr,
		ChatGPTOAuth:     providers.NewChatGPTOAuth(providerStore),
		PendingApprovals: store.NewPendingApprovalStore(db),
		Tasks:            store.NewTaskStore(db),
		ContextProfiles:  store.NewContextProfileStore(db),
		Workflows:        store.NewWorkflowStore(db),
		Wakeups:          store.NewWakeupStore(db),
		Attachments:      store.NewAttachmentStore(db),
		Audit:            func(context.Context, protocol.AuditRecord) {},
		Users:            store.NewUserStore(db),
	}
}

// testRunner is a Runner over testDeps, each edit applied before it is built;
// its Deps are what a test builds an agent on.
func testRunner(ctx context.Context, tb testing.TB, db *bun.DB, edit ...func(*AgentDeps)) *Runner {
	tb.Helper()
	deps := testDeps(tb, db)
	for _, e := range edit {
		e(deps)
	}
	r, err := NewRunner(ctx, db, deps)
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// Every field of AgentDeps is accounted for: a required one missing refuses
// the Runner by name, and the rest are the Runner's own to set.
func TestNewRunnerRefusesAnIncompleteDependencySet(t *testing.T) {
	db := testdb.New(t)
	built := testRunner(t.Context(), t, db).Deps
	fields := reflect.TypeFor[AgentDeps]()
	required := 0
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		deps := testDeps(t, db)
		injected := reflect.ValueOf(deps).Elem().Field(i)
		if injected.IsNil() {
			// Not the composition root's to inject: NewRunner must have set it.
			if reflect.ValueOf(built).Elem().Field(i).IsNil() {
				t.Errorf("%s is neither injected by the composition root nor set by NewRunner", name)
			}
			continue
		}
		required++
		injected.SetZero()
		if _, err := NewRunner(t.Context(), db, deps); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("NewRunner without %s: err = %v, want a refusal naming it", name, err)
		}
	}
	if required == 0 {
		t.Fatal("no field was checked")
	}
	if _, err := NewRunner(t.Context(), db, nil); err == nil {
		t.Error("NewRunner accepted a nil dependency set")
	}
	if _, err := NewRunner(t.Context(), nil, testDeps(t, db)); err == nil {
		t.Error("NewRunner accepted a nil database")
	}
}
