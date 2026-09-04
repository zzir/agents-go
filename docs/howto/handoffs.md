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

`agents.HandoffTo(target)` builds a no-input tool named `transfer_to_<sanitized name>` whose description includes the target's `HandoffDescription`.

## Customizing a handoff

For a custom tool name, an input schema, side effects or dynamic targets, build the `Handoff` struct directly:

```go
type escalationInput struct {
	Reason string `json:"reason" jsonschema:"why the conversation is being escalated"`
}

schema, _ := agents.SchemaFor[escalationInput](true)

h := agents.Handoff{
	ToolName:        "escalate_to_human_review",
	ToolDescription: "Escalate the conversation for human review.",
	InputJSONSchema: schema,
	AgentName:       escalation.Name,
	Target:          escalation,
	OnHandoff: func(ctx context.Context, rc *agents.RunContext, argsJSON string) error {
		var in escalationInput
		_ = json.Unmarshal([]byte(argsJSON), &in)
		log.Printf("escalating: %s", in.Reason)
		return nil // an error here aborts the run
	},
}
```

A handoff whose target depends on the arguments sets `OnInvoke` instead of `Target` and leaves `Target` nil — it is the *static* declaration a registry-rebuilding consumer trusts without invoking callbacks, and a handoff with neither fails the run with a `*UserError` ([spec §2.4](../reference/spec.md#24-handoffs)). A hand-built `Handoff` is strict by default; set `NonStrictSchema: true` only for a schema strict mode cannot express. Every field (`InputFilter`, `IsEnabled`, …) is on [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#Handoff).

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

> Note: the filter receives one flattened `InputHistory` list, not a pre/post split — a filter that needs the boundary can find it by identity.

### Nesting handoff history

For multi-agent chains, `agents.NestHandoffHistory` is a ready-made filter that folds the prior conversation into one compact summary message for the next agent, cutting tokens and tool-call noise:

```go
h := agents.HandoffTo(billing)
h.InputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
```

The default folds the transcript into a single assistant message wrapped in fixed `<CONVERSATION HISTORY>` markers. On a later handoff the filter **flattens** any earlier summary back into its transcript before re-folding, so a chain of handoffs yields one flat summary rather than a summary-of-summaries. Customize via `NestHistoryOptions`:

- `Mapper` — a `HandoffHistoryMapper` that folds the transcript your own way (e.g. call an LLM for a real summary instead of the default JSON-per-line transcript). Only the default summary shape is flattened by later handoffs; a custom mapper's summaries are treated as opaque messages.

The transcript is serialized one JSON item per line, which round-trips through `UnmarshalInputItem` when flattened — a line-delimited format nests reliably, where free text does not.

## Recommended prompts

Models follow handoffs better when the instructions mention them:

```go
triage.Instructions = agents.StaticInstructions(`You are a triage agent for a customer support system.
You can transfer the conversation to specialist agents using the transfer tools.
Transfers are seamless: do not mention or draw attention to them.`)
```

## Semantics worth knowing

Handoff input is validated against the whole `InputJSONSchema` before `OnHandoff` runs, must be a JSON object, and a violation fails the run with a `*ModelBehaviorError` naming the JSON-pointer path ([spec §2.7h](../reference/spec.md#27h-schema-validation)). Function tools in the same turn run **before** the handoff, the **first** of several handoffs wins (the rest get a synthetic "Multiple handoffs detected, ignoring this one." tool output), `OnHandoff` fires before control moves and the target's `OnStart` before its first turn — watch `*AgentUpdatedStreamEvent` for a run-level view — and `RunResult.LastAgent` is the agent that ultimately answered ([spec §2.4](../reference/spec.md#24-handoffs)).
