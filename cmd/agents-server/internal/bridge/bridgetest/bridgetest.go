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
	st := store.NewStores(db)
	r, err := bridge.NewRunner(tb.Context(), db, &bridge.AgentDeps{
		Stores:            st,
		SettingReader:     settings.NewReader(st.Settings),
		GuardrailResolver: guardrails.NewResolver(st.Guardrails),
		McpManager:        mcp,
		SandboxManager:    sandboxMgr,
		ChatGPTOAuth:      providers.NewChatGPTOAuth(st.Providers),
		RecordAudit:       func(context.Context, protocol.AuditRecord) {},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return r
}
