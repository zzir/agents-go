// Package bridgetest builds the bridge for other packages' test suites.
package bridgetest

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// NewRunner returns a Runner over a complete dependency set on db, as the
// composition root builds one, scoped to the test; its Deps field is that set.
// The bridge's own tests carry a copy (they cannot import this package).
func NewRunner(tb testing.TB, db *bun.DB) *bridge.Runner {
	tb.Helper()
	mcp, sandboxMgr := mcpservers.NewManager(tb.Context()), sandboxes.NewManager()
	tb.Cleanup(func() {
		mcp.CloseAll()
		sandboxMgr.CloseAll()
	})
	providerStore := store.NewProviderStore(db)
	r, err := bridge.NewRunner(tb.Context(), db, &bridge.AgentDeps{
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
	})
	if err != nil {
		tb.Fatal(err)
	}
	return r
}
