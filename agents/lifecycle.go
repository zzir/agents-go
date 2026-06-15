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
	// OnLLMStart fires just before the agent's model is invoked, with the
	// resolved system prompt (empty if none) and the input items being sent. It
	// does not fire on a HITL-resumed turn, which reuses the interrupted
	// response instead of calling the model.
	OnLLMStart(ctx context.Context, rc *RunContext, agent *Agent, systemPrompt string, input []TResponseInputItem) error
	// OnLLMEnd fires immediately after the model call returns, with the response.
	OnLLMEnd(ctx context.Context, rc *RunContext, agent *Agent, response *ModelResponse) error
	runHooks()
}

// BaseRunHooks is a no-op RunHooks implementation. Embed it to override only the
// callbacks you care about.
type BaseRunHooks struct{}

// OnAgentStart is a no-op.
func (BaseRunHooks) OnAgentStart(context.Context, *RunContext, *Agent) error { return nil }

// OnAgentEnd is a no-op.
func (BaseRunHooks) OnAgentEnd(context.Context, *RunContext, *Agent, any) error { return nil }

// OnHandoff is a no-op.
func (BaseRunHooks) OnHandoff(context.Context, *RunContext, *Agent, *Agent) error { return nil }

// OnToolStart is a no-op.
func (BaseRunHooks) OnToolStart(context.Context, *RunContext, *Agent, Tool) error { return nil }

// OnToolEnd is a no-op.
func (BaseRunHooks) OnToolEnd(context.Context, *RunContext, *Agent, Tool, any) error {
	return nil
}

// OnLLMStart is a no-op.
func (BaseRunHooks) OnLLMStart(context.Context, *RunContext, *Agent, string, []TResponseInputItem) error {
	return nil
}

// OnLLMEnd is a no-op.
func (BaseRunHooks) OnLLMEnd(context.Context, *RunContext, *Agent, *ModelResponse) error {
	return nil
}
func (BaseRunHooks) runHooks() {}

// AgentHooks receives lifecycle callbacks scoped to a single agent. Embed
// BaseAgentHooks for a no-op default. It mirrors the Python SDK's AgentHooks
// (on_start, on_end, on_handoff, on_tool_start, on_tool_end).
type AgentHooks interface {
	OnStart(ctx context.Context, rc *RunContext, agent *Agent) error
	OnEnd(ctx context.Context, rc *RunContext, agent *Agent, output any) error
	// OnHandoff fires on the receiving agent's hooks when control is handed to
	// it; source is the agent that delegated.
	OnHandoff(ctx context.Context, rc *RunContext, agent, source *Agent) error
	// OnToolStart fires before one of this agent's tools is invoked.
	OnToolStart(ctx context.Context, rc *RunContext, agent *Agent, tool Tool) error
	// OnToolEnd fires after one of this agent's tools returns, with its result.
	OnToolEnd(ctx context.Context, rc *RunContext, agent *Agent, tool Tool, result any) error
	// OnLLMStart fires just before this agent's model is invoked, with the
	// resolved system prompt (empty if none) and the input items being sent.
	OnLLMStart(ctx context.Context, rc *RunContext, agent *Agent, systemPrompt string, input []TResponseInputItem) error
	// OnLLMEnd fires immediately after this agent's model call returns.
	OnLLMEnd(ctx context.Context, rc *RunContext, agent *Agent, response *ModelResponse) error
	agentHooks()
}

// BaseAgentHooks is a no-op AgentHooks implementation.
type BaseAgentHooks struct{}

// OnStart is a no-op.
func (BaseAgentHooks) OnStart(context.Context, *RunContext, *Agent) error { return nil }

// OnEnd is a no-op.
func (BaseAgentHooks) OnEnd(context.Context, *RunContext, *Agent, any) error { return nil }

// OnHandoff is a no-op.
func (BaseAgentHooks) OnHandoff(context.Context, *RunContext, *Agent, *Agent) error { return nil }

// OnToolStart is a no-op.
func (BaseAgentHooks) OnToolStart(context.Context, *RunContext, *Agent, Tool) error { return nil }

// OnToolEnd is a no-op.
func (BaseAgentHooks) OnToolEnd(context.Context, *RunContext, *Agent, Tool, any) error {
	return nil
}

// OnLLMStart is a no-op.
func (BaseAgentHooks) OnLLMStart(context.Context, *RunContext, *Agent, string, []TResponseInputItem) error {
	return nil
}

// OnLLMEnd is a no-op.
func (BaseAgentHooks) OnLLMEnd(context.Context, *RunContext, *Agent, *ModelResponse) error {
	return nil
}
func (BaseAgentHooks) agentHooks() {}

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
		if err := hooks.OnHandoff(ctx, rc, from, to); err != nil {
			return err
		}
	}
	if to.Hooks != nil {
		if err := to.Hooks.OnHandoff(ctx, rc, to, from); err != nil {
			return err
		}
	}
	return nil
}

func callToolStart(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, tool Tool) error {
	if hooks != nil {
		if err := hooks.OnToolStart(ctx, rc, agent, tool); err != nil {
			return err
		}
	}
	if agent.Hooks != nil {
		if err := agent.Hooks.OnToolStart(ctx, rc, agent, tool); err != nil {
			return err
		}
	}
	return nil
}

func callToolEnd(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, tool Tool, result any) error {
	if hooks != nil {
		if err := hooks.OnToolEnd(ctx, rc, agent, tool, result); err != nil {
			return err
		}
	}
	if agent.Hooks != nil {
		if err := agent.Hooks.OnToolEnd(ctx, rc, agent, tool, result); err != nil {
			return err
		}
	}
	return nil
}

func callLLMStart(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, systemPrompt string, input []TResponseInputItem) error {
	if hooks != nil {
		if err := hooks.OnLLMStart(ctx, rc, agent, systemPrompt, input); err != nil {
			return err
		}
	}
	if agent.Hooks != nil {
		if err := agent.Hooks.OnLLMStart(ctx, rc, agent, systemPrompt, input); err != nil {
			return err
		}
	}
	return nil
}

func callLLMEnd(ctx context.Context, hooks RunHooks, agent *Agent, rc *RunContext, response *ModelResponse) error {
	if hooks != nil {
		if err := hooks.OnLLMEnd(ctx, rc, agent, response); err != nil {
			return err
		}
	}
	if agent.Hooks != nil {
		if err := agent.Hooks.OnLLMEnd(ctx, rc, agent, response); err != nil {
			return err
		}
	}
	return nil
}
