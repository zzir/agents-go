# agents-go

**Go agents. Local first.**

The workbench (`agents-server`) is the Go-native agent workbench you run
yourself: run agents and workflows in a sandbox, behind tool approvals; debug
with traces, replay & fork. One binary, your data, solo or as a team. Get it
from
[Releases](https://github.com/zzir/agents-go/releases); the
[workbench manual](https://github.com/zzir/agents-go/blob/main/cmd/agents-server/README.md)
covers flags, the REST API, the WebSocket protocol and the design invariants.

This site documents the SDK underneath — embeddable on its own, and
independent of the workbench ([spec §1.2](spec.md#12-non-goals)).
`agents-go` builds agentic AI apps on
the OpenAI **Responses API** from a small set of primitives and very few
abstractions. It started as a port of the
[OpenAI Agents SDK for Python](https://openai.github.io/openai-agents-python/)
and shares its core concepts, but it now evolves on its own: behavior is
[specified](spec.md), not inherited.

- **Agents**: LLMs configured with instructions, tools, guardrails and handoffs
- **Handoffs**: let an agent delegate the conversation to another agent
- **Guardrails**: validate inputs, outputs and tool calls, in parallel with the run
- **Sessions**: persist conversation history across runs
- **Human-in-the-loop**: pause a run for tool approval and resume it later — even in another process
- **Tracing**: built-in spans for every model call, tool call, handoff and guardrail
- **Sandboxes**: run model-generated code in locked-down Docker containers

The shape is idiomatic Go: generics instead of runtime reflection magic,
`context.Context` for cancellation, errors instead of exceptions, and
`iter.Seq2` for streaming — a run executes on the consumer's goroutine, so
abandoning a stream stops the run rather than leaking one. Where behavior
diverges from the Python SDK it is a decision with a reason; see
[Differences from the Python SDK](migration_from_python.md) for the comparison
and [upstream watch](upstream_watch.md) for what has been reviewed and
declined.

## Installation

```bash
go get github.com/zzir/agents-go
```

Anything with a heavy dependency lives in its own module, so the core stays
dependency-light ([why, module by module](architecture.md#module-boundaries)):

```bash
go get github.com/zzir/agents-go/mcp              # optional: MCP client
go get github.com/zzir/agents-go/models/anthropic # optional: Anthropic backend
go get github.com/zzir/agents-go/sandbox/docker   # optional: Docker sandbox
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
| The workbench | [Manual](https://github.com/zzir/agents-go/blob/main/cmd/agents-server/README.md) — flags, REST API, WebSocket protocol, design invariants |
| Get started | [Quickstart](quickstart.md) |
| Configuration | [Configuration](config.md) |
| Core concepts | [Agents](agents.md) · [Running agents](running_agents.md) · [Results](results.md) |
| Tools | [Tools](tools.md) · [Model context protocol (MCP)](mcp.md) · [Sandbox agents](sandbox.md) · [Skills](skills.md) |
| Orchestration | [Agent orchestration](multi_agent.md) · [Handoffs](handoffs.md) · [Background tasks](tasks.md) |
| Safety | [Guardrails](guardrails.md) · [Human-in-the-loop](human_in_the_loop.md) |
| State | [Sessions](sessions.md) · [Context management](context.md) · [Usage](usage.md) |
| Streaming | [Streaming](streaming.md) |
| Models | [Models](models.md) |
| Observability | [Tracing](tracing.md) · [Logging and diagnostics](logging.md) |
| Testing | [Testing your agents](testing.md) — scripted models, no API key |
| Examples | [Examples](examples.md) |
| API index | [Feature reference](features.md) — every capability mapped to its API |
| Coming from Python? | [Differences from the Python SDK](migration_from_python.md) |
| Architecture | [Architecture](architecture.md) — how the pieces compose, and where the extension points are |
| Behavior spec | [Design spec](spec.md) — the invariants, and why each one is what it is |
| Upstream | [Upstream watch](upstream_watch.md) — what was reviewed from the Python SDK, ported or declined |
