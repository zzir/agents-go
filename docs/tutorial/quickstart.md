# Quickstart

## Create a project

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/zzir/agents-go
export OPENAI_API_KEY=sk-...
```

Anything with a heavy dependency lives in its own module, so the core stays
dependency-light
([why, module by module](../explanation/architecture.md#module-boundaries)).
Add only what you use:

```bash
go get github.com/zzir/agents-go/mcp              # optional: MCP client
go get github.com/zzir/agents-go/models/anthropic # optional: Anthropic backend
go get github.com/zzir/agents-go/sandbox/docker   # optional: Docker sandbox
go get github.com/zzir/agents-go/sessions         # optional: SQLite/Postgres
go get github.com/zzir/agents-go/skills           # optional: Agent Skills
```

## Create your first agent

An agent is a plain struct: instructions, a name, and optional configuration such as tools or a structured output type.

```go
import "github.com/zzir/agents-go/agents"

historyTutor := &agents.Agent{
	Name:               "History Tutor",
	HandoffDescription: "Specialist agent for historical questions",
	Instructions:       agents.StaticInstructions("You provide assistance with historical queries. Explain important events and context clearly."),
}
```

## Add a few more agents

`HandoffDescription` gives the routing agent extra context for deciding where to hand off.

```go
mathTutor := &agents.Agent{
	Name:               "Math Tutor",
	HandoffDescription: "Specialist agent for math questions",
	Instructions:       agents.StaticInstructions("You help with math problems. Show your reasoning step by step."),
}
```

## Define your handoffs

`Handoffs` lists the agents this agent may delegate to. The model sees each as a `transfer_to_<name>` tool.

```go
triage := &agents.Agent{
	Name:         "Triage Agent",
	Instructions: agents.StaticInstructions("You determine which agent to use based on the user's question."),
	Handoffs:     []agents.Handoff{agents.HandoffTo(historyTutor), agents.HandoffTo(mathTutor)},
}
```

## Run the agent loop

```go
import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/models/openai"
)

func main() {
	provider := openai.NewProvider()

	res, err := agents.RunSync(context.Background(), triage, "What is the French Revolution?", agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString()) // answered by the History Tutor
}
```

## Add a guardrail

Guardrails run alongside the first model call and can stop a run before it
wastes tokens. One guardrail declares the stages it inspects, so a single value
can cover the input, the tool arguments and the final output.

{% raw %}
```go
triage.Guardrails = []agents.Guardrail{{
	Name:   "homework_only",
	Stages: []agents.GuardrailStage{agents.StageInput},
	Run: func(ctx context.Context, rc *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
		// Inspect p.Input (or call a cheap classifier model here).
		offTopic := false
		if offTopic {
			return agents.Trip("not a homework question"), nil
		}
		return agents.Allow(nil), nil
	},
}}
```
{% endraw %}

When a guardrail trips, `Run` returns an `*agents.GuardrailTripwireError`;
`tw.Stage()` says which stage fired. See [Guardrails](../howto/guardrails.md) for the
other stages and for substitution.

## Add a function tool

`NewTool` reflects a typed Go function into a strict JSON-schema tool. Struct tags document the parameters.

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up"`
}

weather := agents.NewTool("get_weather", "Look up the current weather for a city.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "Sunny, 23°C in " + args.City, nil
	})

mathTutor.Tools = []*agents.Tool{weather}
```

## Return structured output

Structured output is the same idea in reverse: a Go type on the agent, the
typed value back out of the result. (From here on, `ctx` and `opts` are the
context and the `agents.RunOptions` from the run above.)

```go
type answer struct {
	Summary string   `json:"summary"`
	Sources []string `json:"sources"`
}

historyTutor.OutputType = agents.OutputType[answer]()

res, err := agents.RunSync(ctx, historyTutor, "Who built the Colosseum?", opts)
if err != nil {
	log.Fatal(err)
}
if a, ok := agents.FinalOutputAs[answer](res); ok {
	fmt.Println(a.Summary, a.Sources)
}
```

See [Structured output types](../howto/agents.md#structured-output-types).

## Stream the run

A run *is* an iterator. `Run` returns one plus a control handle; the run
advances as you consume it, so abandoning the loop stops the run instead of
leaking a goroutine.

```go
stream, ctrl := agents.Run(ctx, triage, "tell me about the Roman Republic", opts)
for event, err := range stream {
	if err != nil {
		log.Fatal(err)
	}
	switch e := event.(type) {
	case *agents.RunItemStreamEvent:
		if e.Item.Kind == agents.ItemMessage {
			fmt.Println(e.Item.Text())
		}
	case *agents.RunCompletedEvent:
		fmt.Println("done:", e.Result.FinalOutputString())
	}
}
_ = ctrl // StopAfterTurn, and mid-run input: Steer redirects the current
// exchange, NextTurn rides along with the next turn, FollowUp queues the next
// exchange; Pending reports what was not consumed.
```

See [Streaming](../howto/streaming.md) for the event types and
[Controlling a live run](../howto/streaming.md#controlling-a-live-run).

## Pause for approval

A tool with `NeedsApproval` set pauses the run instead of executing; the paused
state survives a process restart.

```go
weather.NeedsApproval = true

res, err := agents.RunSync(ctx, mathTutor, "What's the weather in Rome?", opts)
if err != nil {
	log.Fatal(err)
}
for len(res.Interruptions) > 0 {
	for _, item := range res.Interruptions {
		res.State.Approve(item, false) // or res.State.Reject(item, false, "no")
	}
	if res, err = agents.ResumeRunSync(ctx, res.State, opts); err != nil {
		log.Fatal(err)
	}
}
```

The paused state serializes to JSON (`res.State.MarshalJSON()`) and rebuilds
with `agents.RunStateFromJSON(data, registry)`, so the approval can happen in
another process — see [Human-in-the-loop](../howto/human_in_the_loop.md).

## Put it all together

See [examples/handoffs](../../examples/handoffs/main.go) and [examples/tools](../../examples/tools/main.go) for complete runnable programs, and:

- [Running agents](../howto/running_agents.md) for the run loop, max turns and run options
- [Results](../howto/results.md) for what comes back from a run
- [Streaming](../howto/streaming.md) to surface events while the run executes
