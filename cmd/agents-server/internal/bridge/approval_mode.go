package bridge

import (
	"context"
	"slices"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// plan is the Plan this build's agent is rewritten with. Its read-only set is
// also what on_change asks outside of — one construction, so the two agree.
func (b *BuildResult) plan() middleware.Plan {
	return middleware.Plan{ReadOnlyTools: append(slices.Clone(middleware.DefaultReadOnlyTools), b.PlanReadOnly...)}
}

// ReadOnlySet is the tools this agent may use while planning, and so the ones
// its "ask before changes" mode does not ask about.
func (b *BuildResult) ReadOnlySet() middleware.ReadOnlySet { return b.plan().ReadOnlySet() }

// applyApprovalMode installs the agent's approval mode on every tool it
// carries — invariant 90. on_change asks for each tool plan mode would deny,
// always for every tool; exec_command keeps its per-command gate in both.
func applyApprovalMode(r *BuildResult) {
	if r.Agent == nil || !r.Approval.Asks() {
		return
	}
	readOnly := r.plan().ReadOnlySet()
	ask := func(t *agents.Tool, fromMCP bool) bool {
		if t.Name == execCommandToolName {
			return false
		}
		return r.Approval.Mode == store.ApprovalModeAlways || !readOnly.Admits(t, fromMCP)
	}
	tools := make([]*agents.Tool, len(r.Agent.Tools))
	for i, t := range r.Agent.Tools {
		tools[i] = t
		if ask(t, false) {
			tools[i] = askBeforeCall(t)
		}
	}
	r.Agent.Tools = tools
	if len(r.Agent.MCPServers) == 0 {
		return
	}
	wrapped := make([]agents.MCPServer, 0, len(r.Agent.MCPServers))
	for _, s := range r.Agent.MCPServers {
		wrapped = append(wrapped, modeMCP{inner: s, ask: ask})
	}
	r.Agent.MCPServers = wrapped
}

// askBeforeCall returns a copy of t that pauses for approval on every call.
// The tool's own predicate still runs for its error and per-call effects; a
// non-error answer is superseded, as the runner treats a listed name.
func askBeforeCall(t *agents.Tool) *agents.Tool {
	asked := *t
	inner := t.NeedsApprovalFunc
	asked.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
		if inner != nil {
			if _, err := inner(ctx, rc, argsJSON, callID); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return &asked
}

// modeMCP applies the approval mode to an MCP server's per-turn listing.
type modeMCP struct {
	inner agents.MCPServer
	ask   func(t *agents.Tool, fromMCP bool) bool
}

func (m modeMCP) Name() string { return m.inner.Name() }
func (m modeMCP) Close() error { return m.inner.Close() }

func (m modeMCP) ListTools(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) ([]*agents.Tool, error) {
	tools, err := m.inner.ListTools(ctx, rc, agent)
	if err != nil {
		return tools, err
	}
	// A fresh slice: the inner server may hand out a cached one.
	out := make([]*agents.Tool, 0, len(tools))
	for _, t := range tools {
		if m.ask(t, true) {
			t = askBeforeCall(t)
		}
		out = append(out, t)
	}
	return out, nil
}

var _ agents.MCPServer = modeMCP{}
