package agents

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// resolveModel returns the Model for the given agent, honoring (in order) the
// agent's explicit ModelImpl, the run-level Model override, then the provider.
func (r *runner) resolveModel(agent *Agent) (Model, error) {
	if agent.ModelImpl != nil {
		return agent.ModelImpl, nil
	}
	if r.opts.Model.Override != nil {
		return r.opts.Model.Override, nil
	}
	if r.opts.Model.Provider != nil {
		return r.opts.Model.Provider.GetModel(agent.Model)
	}
	return nil, NewUserError("no model available: set Agent.ModelImpl, RunOptions.Model.Override, or RunOptions.Model.Provider")
}

// resolveSettings merges the run-level settings override over the agent's own.
func (r *runner) resolveSettings(agent *Agent) *ModelSettings {
	base := agent.ModelSettings
	if base == nil {
		base = &ModelSettings{}
	}
	s := base.Resolve(r.opts.Model.Settings)
	// Once an agent has called tools, leave tool_choice unset on its later
	// turns so a "required"/specific-tool setting cannot force an infinite
	// tool-call loop (the Python SDK's reset_tool_choice behavior).
	if !agent.DisableToolChoiceReset && s.ToolChoice != "" && r.toolsUsedBy[agent.Name] {
		s.ToolChoice = ""
	}
	return s
}

// enabledTools returns the agent's tools, filtered by any IsEnabled predicate
// and augmented with the tools exposed by the agent's MCP servers.
func (r *runner) enabledTools(ctx context.Context, agent *Agent) ([]*FunctionTool, error) {
	out := make([]*FunctionTool, 0, len(agent.Tools))
	for _, t := range agent.Tools {
		enabled, err := t.enabled(ctx, r.rc, agent)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		// A deferred tool waits until something discloses it. It is checked
		// after IsEnabled so a disclosed tool that is also disabled stays
		// hidden — disclosure opens a door, it does not force one.
		if t.Deferred && !r.disclosed[t.Name] {
			continue
		}
		out = append(out, t)
	}
	for _, server := range agent.MCPServers {
		mcpTools, err := server.ListTools(ctx, r.rc, agent)
		if err != nil {
			// Failing the turn, not skipping the server: with its tools quietly
			// missing, the model's next call to one it used a turn ago becomes
			// a "tool not found" error blamed on the model. A listing failure
			// is this run's failure, named as such.
			return nil, fmt.Errorf("listing tools of MCP server for agent %q: %w", agent.Name, err)
		}
		out = append(out, mcpTools...)
	}
	// Reject duplicate tool names instead of silently letting the last one
	// shadow the others in the runner's dispatch map.
	seen := make(map[string]bool, len(out))
	for _, t := range out {
		name := t.Name
		if name == "" {
			continue
		}
		if seen[name] {
			return nil, NewUserError("duplicate tool name %q on agent %q: tool names must be unique across Agent.Tools and MCP server tools", name, agent.Name)
		}
		seen[name] = true
	}
	return out, nil
}

// enabledHandoffs returns the agent's handoffs, filtered by any IsEnabled
// predicate (nil means enabled; a predicate error aborts the run). A disabled
// handoff is not offered to the model and cannot be invoked.
func (r *runner) enabledHandoffs(ctx context.Context, agent *Agent) ([]Handoff, error) {
	out := make([]Handoff, 0, len(agent.Handoffs))
	for _, h := range agent.Handoffs {
		if h.IsEnabled != nil {
			ok, err := h.IsEnabled(ctx, r.rc, agent)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, h)
	}
	return out, nil
}

func agentOutputSchema(agent *Agent) OutputSchema {
	if agent.OutputType != nil {
		return agent.OutputType
	}
	return PlainTextOutput()
}

// toolsUsedList returns the agent names in a tool-use tracker as a slice, for
// carrying the tool_choice reset across an interrupt/resume in RunState.
func toolsUsedList(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}

// markToolsUsed records that agent called tools this run (for tool_choice reset).
func (r *runner) markToolsUsed(agent *Agent) {
	if r.toolsUsedBy == nil {
		r.toolsUsedBy = map[string]bool{}
	}
	r.toolsUsedBy[agent.Name] = true
}
