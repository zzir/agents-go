# agents-go

**Go agents. Local first.**

The workbench (`agents-server`) is the Go-native agent workbench you run
yourself: run agents and workflows in a sandbox, behind tool approvals; debug
with traces, replay & fork. One binary, your data, solo or as a team. Get it
from
[Releases](https://github.com/zzir/agents-go/releases); the
[workbench manual](../cmd/agents-server/README.md)
covers flags, the REST API, the WebSocket protocol and the design invariants.

These pages document the SDK underneath — embeddable on its own, and
independent of the workbench ([spec §1.2](explanation/scope.md#12-non-goals)).
`agents-go` builds agentic AI apps on
the OpenAI **Responses API** from a small set of primitives and very few
abstractions. It started as a port of the
[OpenAI Agents SDK for Python](https://openai.github.io/openai-agents-python/)
and shares its core concepts, but it now evolves on its own: behavior is
[specified](reference/spec.md), not inherited.

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
[Differences from the Python SDK](explanation/migration_from_python.md) for the comparison
and [upstream watch](explanation/upstream_watch.md) for what has been reviewed and
declined.

## Installation

```bash
go get github.com/zzir/agents-go
```

Anything with a heavy dependency lives in its own module, so the core stays
dependency-light ([why, module by module](explanation/architecture.md#module-boundaries)):

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

The pages are sorted by what you came for.

### Tutorial — learn by doing

| Page | |
|---|---|
| [Quickstart](tutorial/quickstart.md) | Build and run your first agent |
| [Examples](examples.md) | Runnable programs, one per capability |

### How-to — solve one problem

| Area | Pages |
|---|---|
| Core | [Agents](howto/agents.md) · [Running agents](howto/running_agents.md) · [Results](howto/results.md) · [Configuration](howto/config.md) |
| Tools | [Tools](howto/tools.md) · [MCP](howto/mcp.md) · [Sandbox agents](howto/sandbox.md) · [Skills](howto/skills.md) |
| Orchestration | [Agent orchestration](howto/multi_agent.md) · [Handoffs](howto/handoffs.md) · [Background tasks](howto/tasks.md) |
| Safety | [Guardrails](howto/guardrails.md) · [Human-in-the-loop](howto/human_in_the_loop.md) |
| State | [Sessions](howto/sessions.md) · [Context management](howto/context.md) · [Usage](howto/usage.md) |
| Streaming | [Streaming](howto/streaming.md) |
| Models | [Models](howto/models.md) |
| Observability | [Tracing](howto/tracing.md) · [Logging and diagnostics](howto/logging.md) |
| Testing | [Testing your agents](howto/testing.md) — scripted models, no API key |

### Reference — look something up

| Page | |
|---|---|
| [Design spec](reference/spec.md) | The behavioral invariants — what is always true |
| [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go) | Every exported symbol, always in sync with the code |

### Explanation — understand why

| Page | |
|---|---|
| [Architecture](explanation/architecture.md) | How the pieces compose, and where the extension points are |
| [Design decisions](explanation/decisions.md) | Settled decisions, each with the reason — read before reopening one |
| [Scope](explanation/scope.md) | What this is, what it deliberately is not |
| [Differences from the Python SDK](explanation/migration_from_python.md) | For readers arriving from `openai-agents-python` |
| [Upstream watch](explanation/upstream_watch.md) | What was reviewed from the Python SDK, ported or declined |

### The workbench

[Manual](../cmd/agents-server/README.md) — flags, deployment, the REST and WebSocket surfaces, and the design invariants the panels obey.
