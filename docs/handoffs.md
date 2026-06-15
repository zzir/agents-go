# Handoffs

Handoffs let an agent delegate the rest of the run to another agent. The model sees each handoff as a tool named `transfer_to_<agent_name>`; when it calls one, the runner switches the active agent and continues the loop with the full conversation.

## Creating a handoff

```go
billing := &agents.Agent{Name: "Billing agent", Instructions: agents.StaticInstructions("…")}
refund := &agents.Agent{Name: "Refund agent", HandoffDescription: "Handles refund requests end to end.", Instructions: agents.StaticInstructions("…")}

triage := &agents.Agent{
	Name:     "Triage agent",
	Handoffs: []agents.Handoff{agents.HandoffTo(billing), agents.HandoffTo(refund)},
}
```

`agents.HandoffTo(target)` is the Go counterpart of Python's `handoff(agent)`: it builds a no-input tool named `transfer_to_<sanitized name>` whose description includes the target's `HandoffDescription`.

## Customizing a handoff

For a custom tool name, an input schema, side effects or dynamic targets, build the `Handoff` struct directly:

```go
type escalationInput struct {
	Reason string `json:"reason" jsonschema:"why the conversation is being escalated"`
}

schema, _ := agents.SchemaFor[escalationInput](true)

h := agents.Handoff{
	ToolName:         "escalate_to_human_review",
	ToolDescription:  "Escalate the conversation for human review.",
	InputJSONSchema:  schema,
	StrictJSONSchema: true,
	AgentName:        escalation.Name,
	OnInvoke: func(ctx context.Context, rc *agents.RunContext, argsJSON string) (*agents.Agent, error) {
		return escalation, nil // may pick a target dynamically
	},
	OnHandoff: func(ctx context.Context, rc *agents.RunContext, argsJSON string) error {
		var in escalationInput
		_ = json.Unmarshal([]byte(argsJSON), &in)
		log.Printf("escalating: %s", in.Reason)
		return nil // an error here aborts the run
	},
}
```

| Field | Purpose |
|---|---|
| `ToolName` / `ToolDescription` | What the model sees |
| `InputJSONSchema` / `StrictJSONSchema` | Optional typed handoff input |
| `OnInvoke` | Returns the agent to switch to (required) |
| `OnHandoff` | Side-effect callback when the handoff fires (e.g. prefetch data) |
| `InputFilter` | Rewrites the conversation the next agent sees (below) |
| `IsEnabled` | Gates whether the handoff is offered to the model this run |

## Input filters

By default the next agent sees the entire conversation. An `InputFilter` rewrites it — for example to drop earlier tool noise before delegating:

```go
h := agents.HandoffTo(faq)
h.InputFilter = func(d agents.HandoffInputData) agents.HandoffInputData {
	d.InputHistory = removeToolItems(d.InputHistory)
	return d
}
```

`HandoffInputData.InputHistory` is the full conversation as input items, up to and including the handoff. The filter affects only what the next agent sees — what is saved to a [session](sessions.md) is unaffected.

> Note: the Go filter receives one flattened `InputHistory` list rather than Python's three-part `input_history` / `pre_handoff_items` / `new_items` split.

### Nesting handoff history

For multi-agent chains, `agents.NestHandoffHistory` is a ready-made filter that folds the prior conversation into one compact summary message for the next agent (mirroring Python's `nest_handoff_history`), cutting tokens and tool-call noise:

```go
h := agents.HandoffTo(billing)
h.InputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
```

The default folds the transcript into a single assistant message wrapped in `<CONVERSATION HISTORY>` markers. On a later handoff the filter **flattens** any earlier summary back into its transcript before re-folding, so a chain of handoffs yields one flat summary rather than a summary-of-summaries. Customize via `NestHistoryOptions`:

- `Mapper` — a `HandoffHistoryMapper` that folds the transcript your own way (e.g. call an LLM for a real summary instead of the default JSON-per-line transcript).
- `StartMarker` / `EndMarker` — override the wrapper markers (a custom `Mapper` must reuse them for flattening to work).

The transcript is serialized one JSON item per line, which round-trips through `UnmarshalInputItem` when flattened — Go uses this in place of Python's looser text format for reliable nesting.

## Recommended prompts

As in Python, models follow handoffs better when the instructions mention them:

```go
triage.Instructions = agents.StaticInstructions(`You are a triage agent for a customer support system.
You can transfer the conversation to specialist agents using the transfer tools.
Transfers are seamless: do not mention or draw attention to them.`)
```

## Semantics worth knowing

- Function tools requested in the same turn run **before** the handoff executes.
- If the model requests several handoffs in one turn, the **first** wins; the others receive a synthetic "Multiple handoffs detected, ignoring this one." tool output.
- Run-level `OnHandoff` hooks and the receiving agent's `OnHandoff` agent hook both fire on every handoff.
- `RunResult.LastAgent` is the agent that ultimately answered — useful for routing the user's next message.
