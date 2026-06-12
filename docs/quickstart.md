# Quickstart

## Create a project

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/zzir/agents-go
export OPENAI_API_KEY=sk-...
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

	res, err := agents.Run(context.Background(), triage, "What is the French Revolution?", agents.RunOptions{
		ModelProvider: provider,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString()) // answered by the History Tutor
}
```

## Add a guardrail

Guardrails run alongside the first model call and can stop a run before it wastes tokens.

```go
triage.InputGuardrails = []agents.InputGuardrail{{
	Name: "homework_only",
	Run: func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, input []agents.TResponseInputItem) (agents.GuardrailFunctionOutput, error) {
		// Inspect the input (or call a cheap classifier model here).
		offTopic := false
		return agents.GuardrailFunctionOutput{TripwireTriggered: offTopic}, nil
	},
}}
```

When a tripwire fires, `Run` returns an `*agents.InputGuardrailTripwireError`.

## Add a function tool

`NewFunctionTool` reflects a typed Go function into a strict JSON-schema tool. Struct tags document the parameters.

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up"`
}

weather := agents.NewFunctionTool("get_weather", "Look up the current weather for a city.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "Sunny, 23°C in " + args.City, nil
	})

mathTutor.Tools = []agents.Tool{weather}
```

## Put it all together

See [examples/handoffs](../examples/handoffs/main.go) and [examples/tools](../examples/tools/main.go) for complete runnable programs, and:

- [Running agents](running_agents.md) for the run loop, max turns and run options
- [Results](results.md) for what comes back from a run
- [Streaming](streaming.md) to surface events while the run executes
