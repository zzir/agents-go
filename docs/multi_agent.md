# Orchestrating multiple agents

Orchestration is deciding which agents run, in what order, and how they decide what happens next. There are two main approaches, freely mixable:

1. **Let the LLM decide**: give a capable agent tools and handoffs and let it plan.
2. **Orchestrate via code**: you decide the flow; agents are well-scoped subroutines.

## Orchestrating via LLM

An agent equipped with tools, [handoffs](handoffs.md) and clear instructions can plan autonomously: research with tools, delegate specialist work, write results somewhere. This is the most flexible pattern. Tactics that pay off:

1. Invest in prompts: state available tools, constraints, and how to operate.
2. Monitor with [tracing](tracing.md) and iterate.
3. Let the agent self-improve: loop, critique, retry ([guardrails](guardrails.md) catch the failure modes).
4. Use specialist agents that excel at one task over one generalist.

```go
research := &agents.Agent{Name: "Researcher", Tools: []*agents.Tool{webLookup}}
writer := &agents.Agent{Name: "Writer", Instructions: agents.StaticInstructions("Write the final report.")}

planner := &agents.Agent{
	Name:         "Planner",
	Instructions: agents.StaticInstructions("Plan the work, research as needed, then hand off to the writer."),
	Tools:        []*agents.Tool{webLookup},
	Handoffs:     []agents.Handoff{agents.HandoffTo(writer)},
}
```

## Agents as tools

Where a handoff *transfers* the conversation, `AsTool` keeps the orchestrator in charge: the sub-agent runs on just the input the orchestrator passes, returns its final output as the tool result, and the orchestrator continues.

```go
spanish := &agents.Agent{Name: "spanish_agent", Instructions: agents.StaticInstructions("Translate the message to Spanish.")}
french := &agents.Agent{Name: "french_agent", Instructions: agents.StaticInstructions("Translate the message to French.")}

orchestrator := &agents.Agent{
	Name: "orchestrator",
	Instructions: agents.StaticInstructions(
		"You are a translation agent. Use the tools to translate; for multiple languages, call the relevant tools."),
	Tools: []*agents.Tool{
		spanish.AsTool(agents.AgentToolConfig{Name: "translate_to_spanish", Description: "Translate the user's message to Spanish"}),
		french.AsTool(agents.AgentToolConfig{Name: "translate_to_french", Description: "Translate the user's message to French"}),
	},
}
```

`AgentToolConfig` options:

| Field | Purpose |
|---|---|
| `Name` / `Description` | What the calling model sees (name defaults to the sanitized agent name) |
| `CustomOutputExtractor` | Derive the tool's string result from the nested `*RunResult` (its `AgentToolInvocation` identifies the originating call) |
| `IsEnabled` | Hide the tool from the model per run |
| `NeedsApproval` / `NeedsApprovalFunc` | Make the agent tool itself a human-approval gate |
| `FailureErrorFunction` | Override how a failed nested run is rendered back to the model |
| `ModifyRunOptions` | Configure the nested run's `RunOptions` — session, turn budget, conversation, model, guardrails |
| `OnStream` | Stream the nested run's events to a callback (see below) |
| `InputBuilder` | Control how arguments render into the nested input; the schema check still applies (`agents.AgentToolInputWithSchema` attaches the full schema) |

The config configures the **tool surface**; everything about the nested run
itself goes through `ModifyRunOptions`:

```go
sub.AsTool(agents.AgentToolConfig{
	Name: "specialist",
	ModifyRunOptions: func(o *agents.RunOptions) {
		o.Conversation.Session = sess // conversation state of its own
		o.Exec.MaxTurns = 5           // nested turn budget
	},
})
```

**Streaming a nested run.** Setting `OnStream` switches the nested run to streaming: every event (raw model deltas, run items, agent updates) is delivered as an `AgentToolStreamEvent` carrying the current nested agent and the originating tool call. Events dispatch from a background goroutine so a slow callback never stalls the run; a panic in the callback is recovered, and a canceled parent does not wait for the callback backlog.

**Typed parameters.** `AgentAsTool[Params](agent, cfg)` replaces the default `{input: string}` schema with one reflected from `Params` (like `NewTool`). Both constructors validate the model's arguments against the schema the tool advertises before the nested run starts — a missing key, a wrong type or a violated enum goes back to the model as a tool error to self-correct, never through to the sub-agent as its prompt. The arguments render into the nested input with a structured preamble and the JSON payload, plus a schema summary when any field carries a description — or the full JSON schema with `InputBuilder: agents.AgentToolInputWithSchema` — or through your own `InputBuilder`.

The nested run inherits the parent's model provider, model override, model settings, tracer and log configuration through the run context, so sub-agents need no provider of their own. Its spans join the parent's trace and its log records carry the sub-agent's name; its usage is tracked separately. If the model calls several agent-tools in one turn they run **concurrently** — like any other function tools.

**State isolation.** The nested run never inherits the parent run's conversation state: the sub-agent sees only the input the orchestrator passes, and nothing it does is written to the parent's `Session`. To give the nested run state of its own, set a session or conversation via `ModifyRunOptions` (one strategy at a time, same as a top-level run); to share client-side history with the parent, pass the parent's `Session` there. Python's `previous_response_id` option has no Go counterpart ([differences](migration_from_python.md)).

Without a `CustomOutputExtractor`, the tool result is the nested run's final output — as a string for plain-text agents, or the JSON payload for structured ones. When the final output is empty, it falls back to the last non-empty assistant message, then the last non-empty string tool output.

**Human-in-the-loop through an agent tool.** If a tool *inside* the sub-agent needs approval ([Human-in-the-loop](human_in_the_loop.md)), the nested run pauses and its approval **surfaces as the orchestrator run's own interruption** — `RunResult.Interruptions` carries the nested tool's approval item. Approve or reject it on `RunResult.State` and `ResumeRun` as usual; the orchestrator continues the paused nested run (applying your decision) instead of restarting it, then finishes the parent turn. The paused nested state is serialized recursively inside the parent's `RunState` JSON, so a state persisted and resumed in another process also continues the nested run mid-approval — provided the agent registry passed to `RunStateFromJSON` contains every agent involved, including the sub-agents.

## Orchestrating via code

Plain Go is often the clearest orchestrator — deterministic, testable, cheap:

```go
// Chain: outline -> approve -> write
outline, err := agents.RunSync(ctx, outliner, topic, opts)
if err != nil { return err }

check, err := agents.RunSync(ctx, reviewer, outline.FinalOutputString(), opts)
if err != nil { return err }
if verdict, _ := agents.FinalOutputAs[Verdict](check); !verdict.Good {
	return fmt.Errorf("outline rejected: %s", verdict.Reason)
}

story, err := agents.RunSync(ctx, writer, outline.FinalOutputString(), opts)
```

Two more patterns worth naming:

- **Structured decisions**: use [`OutputType`](agents.md#structured-output-types) to get a typed verdict you can branch on.
- **Chaining**: feed one agent's output into the next.
- **Evaluate-and-retry**: loop a worker and a critic agent until the critic passes.
- **Fan-out**: Go makes parallel agents natural — run several `agents.Run` calls in goroutines (e.g. with `errgroup`) and join the results.

```go
g, gctx := errgroup.WithContext(ctx)
results := make([]*agents.RunResult, len(questions))
for i, q := range questions {
	g.Go(func() error {
		res, err := agents.RunSync(gctx, analyst, q, opts)
		results[i] = res
		return err
	})
}
if err := g.Wait(); err != nil { /* … */ }
```
