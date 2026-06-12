package agents

import "context"

// RunHooks receives lifecycle callbacks for an entire run, across every agent.
// Embed BaseRunHooks to implement only the callbacks you need.
type RunHooks interface {
	// OnAgentStart fires before an agent first executes (and again after a
	// handoff into it).
	OnAgentStart(ctx context.Context, rc *RunContext, agent *Agent) error
	// OnAgentEnd fires when an agent produces a final output that ends the run.
	OnAgentEnd(ctx context.Context, rc *RunContext, agent *Agent, output any) error
	// OnHandoff fires when control passes from one agent to another.
	OnHandoff(ctx context.Context, rc *RunContext, from, to *Agent) error
	// OnToolStart fires before a tool is invoked.
	OnToolStart(ctx context.Context, rc *RunContext, agent *Agent, tool Tool) error
	// OnToolEnd fires after a tool returns, with its result.
	OnToolEnd(ctx context.Context, rc *RunContext, agent *Agent, tool Tool, result any) error
	runHooks()
}

// BaseRunHooks is a no-op RunHooks implementation. Embed it to override only the
// callbacks you care about.
type BaseRunHooks struct{}

func (BaseRunHooks) OnAgentStart(context.Context, *RunContext, *Agent) error      { return nil }
func (BaseRunHooks) OnAgentEnd(context.Context, *RunContext, *Agent, any) error   { return nil }
func (BaseRunHooks) OnHandoff(context.Context, *RunContext, *Agent, *Agent) error { return nil }
func (BaseRunHooks) OnToolStart(context.Context, *RunContext, *Agent, Tool) error { return nil }
func (BaseRunHooks) OnToolEnd(context.Context, *RunContext, *Agent, Tool, any) error {
	return nil
}
func (BaseRunHooks) runHooks() {}

// AgentHooks receives lifecycle callbacks scoped to a single agent. Embed
// BaseAgentHooks for a no-op default.
type AgentHooks interface {
	OnStart(ctx context.Context, rc *RunContext, agent *Agent) error
	OnEnd(ctx context.Context, rc *RunContext, agent *Agent, output any) error
	agentHooks()
}

// BaseAgentHooks is a no-op AgentHooks implementation.
type BaseAgentHooks struct{}

func (BaseAgentHooks) OnStart(context.Context, *RunContext, *Agent) error    { return nil }
func (BaseAgentHooks) OnEnd(context.Context, *RunContext, *Agent, any) error { return nil }
func (BaseAgentHooks) agentHooks()                                           {}

// The call* helpers invoke both run-level and agent-level hooks, tolerating nil.

func callAgentStart(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext) error {
	if hooks != nil {
		if err := hooks.OnAgentStart(ctx, rc, agent); err != nil {
			return err
		}
	}
	if agent.Hooks != nil {
		if err := agent.Hooks.OnStart(ctx, rc, agent); err != nil {
			return err
		}
	}
	return nil
}

func callAgentEnd(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, output any) error {
	if hooks != nil {
		if err := hooks.OnAgentEnd(ctx, rc, agent, output); err != nil {
			return err
		}
	}
	if agent.Hooks != nil {
		if err := agent.Hooks.OnEnd(ctx, rc, agent, output); err != nil {
			return err
		}
	}
	return nil
}

func callHandoff(ctx context.Context, hooks RunHooks, from, to *Agent, rc *RunContext) error {
	if hooks != nil {
		return hooks.OnHandoff(ctx, rc, from, to)
	}
	return nil
}

func callToolStart(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, tool Tool) error {
	if hooks != nil {
		return hooks.OnToolStart(ctx, rc, agent, tool)
	}
	return nil
}

func callToolEnd(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, tool Tool, result any) error {
	if hooks != nil {
		return hooks.OnToolEnd(ctx, rc, agent, tool, result)
	}
	return nil
}
