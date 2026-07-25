# OpenAI Agents SDK for Go

`agents-go` is a Go port of the [OpenAI Agents SDK for Python](https://openai.github.io/openai-agents-python/) (tracking v0.18.2). It lets you build agentic AI apps with a small set of primitives and very few abstractions:

- **Agents**: LLMs configured with instructions, tools, guardrails and handoffs
- **Handoffs**: let an agent delegate the conversation to another agent
- **Guardrails**: validate inputs, outputs and tool calls, in parallel with the run
- **Sessions**: persist conversation history across runs
- **Human-in-the-loop**: pause a run for tool approval and resume it later — even in another process
- **Tracing**: built-in spans for every model call, tool call, handoff and guardrail
- **Sandboxes**: run model-generated code in locked-down Docker containers

The SDK follows the Python design closely — same run loop, same item model, same defaults — while staying idiomatic Go: generics instead of runtime reflection magic, `context.Context` for cancellation, errors instead of exceptions. See [Differences from the Python SDK](migration_from_python.md) for the complete comparison.

## Installation

```bash
go get github.com/zzir/agents-go
```

The sandbox backends, SQL sessions and skills live in separate modules so the core stays dependency-light:

```bash
go get github.com/zzir/agents-go/sandbox/docker   # optional
go get github.com/zzir/agents-go/sessions         # optional: SQLite/Postgres
go get github.com/zzir/agents-go/skills           # optional: Agent Skills
```

## Hello world

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("You are a concise, helpful assistant."),
	}

	res, err := agents.RunSync(context.Background(), agent, "Write a haiku about Go.", agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
```

(Set the `OPENAI_API_KEY` environment variable before running.)

## Documentation

| Topic | Page |
|---|---|
| Get started | [Quickstart](quickstart.md) |
| Configuration | [Configuration](config.md) |
| Core concepts | [Agents](agents.md) · [Running agents](running_agents.md) · [Results](results.md) |
| Tools | [Tools](tools.md) · [Model context protocol (MCP)](mcp.md) · [Sandbox agents](sandbox.md) · [Skills](skills.md) |
| Orchestration | [Agent orchestration](multi_agent.md) · [Handoffs](handoffs.md) |
| Safety | [Guardrails](guardrails.md) · [Human-in-the-loop](human_in_the_loop.md) |
| State | [Sessions](sessions.md) · [Context management](context.md) · [Usage](usage.md) |
| Streaming | [Streaming](streaming.md) |
| Models | [Models](models.md) |
| Observability | [Tracing](tracing.md) |
| Examples | [Examples](examples.md) |
| API index | [Feature reference](features.md) — every capability mapped to its API |
| Coming from Python? | [Differences from the Python SDK](migration_from_python.md) |
