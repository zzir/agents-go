# Quickstart

> **Pre-1.0 API notice.** Until v1.0.0 a minor release may rename or remove
> exported identifiers — pin the version. Breaking renames are batched, and the
> [release notes](https://github.com/zzir/agents-go/releases) carry every old
> spelling beside the new ([decisions §5.8](../explanation/decisions.md#58-public-api-compatibility-begins-at-v100)).

## Create a project

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/zzir/agents-go
export OPENAI_API_KEY=sk-...
```

That is the whole core. The capabilities with a heavy dependency are their
own modules, each a further `go get` when you reach for it — the list, and
why it is split that way, is in
[Architecture](../explanation/architecture.md#module-boundaries).

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

A run *is* an iterator: `Run` returns one plus a control handle, and abandoning
the loop stops the run ([Streaming](../howto/streaming.md#the-run-happens-on-your-goroutine)).

```go
stream, ctrl := agents.Run(ctx, triage, "tell me about the Roman Republic", opts)
for event, err := range stream {
	if err != nil {
		log.Fatal(err)
	}
	if e, ok := event.(*agents.RunCompletedEvent); ok {
		fmt.Println("done:", e.Result.FinalOutputString())
	}
}
```

`ctrl` stops the run after the current turn or steers it mid-flight —
[Controlling a live run](../howto/streaming.md#controlling-a-live-run); the
event types are in [Streaming](../howto/streaming.md#event-types).

## Pause for approval

A tool with `NeedsApproval` set pauses the run instead of executing; the paused
state survives a process restart.

```go
weather.NeedsApproval = true

res, err := agents.RunSync(ctx, mathTutor, "What's the weather in Rome?", opts)
for err == nil && len(res.Interruptions) > 0 {
	for _, item := range res.Interruptions {
		res.State.Approve(item, false) // or res.State.Reject(item, false, "no")
	}
	res, err = agents.ResumeRunSync(ctx, res.State, opts)
}
```

The paused state serializes to JSON and rebuilds in another process — see
[Human-in-the-loop](../howto/human_in_the_loop.md).

## Put it all together

See [examples/handoffs](../../examples/handoffs/main.go) and [examples/tools](../../examples/tools/main.go) for complete runnable programs, and:

- [Running agents](../howto/running_agents.md) for the run loop, max turns and run options
- [Results](../howto/results.md) for what comes back from a run
- [Streaming](../howto/streaming.md) to surface events while the run executes
