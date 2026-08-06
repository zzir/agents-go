<div align="center">

# agents-go

**Build production AI agents in Go** — tools, handoffs, guardrails, sessions,
human-in-the-loop, streaming, tracing.

Built on the OpenAI **Responses API**, with an **Anthropic Messages API**
provider that translates at the model boundary. Started as a port of the
[OpenAI Agents SDK](https://github.com/openai/openai-agents-python), now evolving
on its own — behavior is [specified](docs/spec.md), not inherited.

[![Go Reference](https://pkg.go.dev/badge/github.com/zzir/agents-go.svg)](https://pkg.go.dev/github.com/zzir/agents-go)
[![CI](https://github.com/zzir/agents-go/actions/workflows/ci.yml/badge.svg)](https://github.com/zzir/agents-go/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Documentation](https://zzir.github.io/agents-go/) ·
[Architecture](docs/architecture.md) ·
[Feature reference](docs/features.md) ·
[Examples](docs/examples.md) ·
[Coming from Python?](docs/migration_from_python.md)

</div>

## Why agents-go?

- **Specified behavior** — the run loop, guardrail timing, persistence
  boundaries and budget semantics are written down in [spec.md](docs/spec.md),
  not left to the reader to infer.
- **Type-safe by construction** — tools are generic Go functions; argument
  schemas and structured outputs come from your structs, not runtime dicts.
- **Durable human-in-the-loop** — pause a run for approval, serialize its state
  to JSON, resume it later — even in another process.
- **Production plumbing built in** — retry, fallback, and multi-provider
  routing as composable model decorators; tracing spans for every step.
- **Testable without a key** — `agentstest` scripts the model, so your agent's
  tools and decisions are covered by fast offline tests.
- **Dependency-light core** — one small module. The MCP client, Docker/SSH
  sandboxes, SQL sessions, and skills are opt-in submodules.
- **Batteries included** — MCP client *and* server, code-execution sandboxes,
  web search, Agent Skills, and a full web app on top of the SDK.

## Install

```bash
go get github.com/zzir/agents-go
```

Requires Go 1.26+. Optional backends are separate modules — see
[Packages](docs/features.md#packages).

## Quick start

```go
package main

import (
	"context"
	"fmt"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("You are a helpful assistant."),
		Model:        "gpt-4o",
	}

	res, err := agents.RunSync(context.Background(), agent, "Hello!", agents.RunOptions{
		// Provider reads OPENAI_API_KEY.
		Model: agents.ModelOptions{Provider: openai.NewProvider()},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.FinalOutputString())
}
```

## A taste of the API

**Typed tools.** A tool is a plain Go function; its argument struct is
reflected into a strict-mode JSON schema for the model:

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"the city"`
}

getWeather := agents.NewTool("get_weather", "Look up the weather.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "sunny in " + args.City, nil
	})

agent := &agents.Agent{Name: "bot", Model: "gpt-4o", Tools: []*agents.Tool{getWeather}}
```

Structured output is the same idea in reverse: `OutputType[T]()` on the agent,
`FinalOutputAs[T](res)` on the result — see [Agents](docs/agents.md).

**Streaming.** A run *is* an iterator. `Run` returns one plus a control handle;
the run advances as you consume it, so abandoning the loop stops the run instead
of leaking a goroutine.

```go
stream, ctrl := agents.Run(ctx, agent, "tell me a story", opts)
for event, err := range stream {
	if err != nil { panic(err) }
	switch e := event.(type) {
	case *agents.RunItemStreamEvent:
		if e.Item.Kind == agents.ItemMessage {
			fmt.Println(e.Item.Text())
		}
	case *agents.RunCompletedEvent:
		fmt.Println("done:", e.Result.FinalOutputString())
	}
}
_ = ctrl // StopAfterTurn, Phase, CurrentAgent, CurrentTurn — and mid-run input:
// ctrl.Steer("…") redirects the current exchange, ctrl.NextTurn("…") lands at
// the next turn boundary, ctrl.FollowUp("…") queues the next exchange.
```

**Human-in-the-loop.** Runs pause for approval and survive process restarts:

```go
tool.NeedsApproval = true

res, _ := agents.RunSync(ctx, agent, "delete everything", opts)
for len(res.Interruptions) > 0 {
	for _, item := range res.Interruptions {
		res.State.Approve(item, false) // or res.State.Reject(item, false, "no")
	}
	res, _ = agents.ResumeRunSync(ctx, res.State, opts)
}
```

The paused state serializes to JSON (`res.State.MarshalJSON()`) and rebuilds
with `agents.RunStateFromJSON(data, registry)` for cross-process approvals.

## What's inside

- [Tools](docs/tools.md) — typed function tools, agents-as-tools, multimodal
  tool output, per-tool approval and guardrails
- [Handoffs](docs/handoffs.md) — triage and delegate between agents
- [Guardrails](docs/guardrails.md) — input, output, and tool-level checks;
  a tripwire halts the run
- [Human-in-the-loop](docs/human_in_the_loop.md) — durable approvals across
  processes
- [Sessions](docs/sessions.md) — in-memory, JSONL file, SQLite/Postgres, or
  OpenAI server-side history; append-only entries, branching, and crash recovery
- [Compaction](docs/sessions.md#run-level-compaction) — pluggable strategies
  keep a long conversation in budget; a context-overflow error compacts and
  retries the turn instead of failing the run
- [Streaming](docs/streaming.md) — token and item events as a range-able
  iterator; steer a live run or queue follow-ups through `RunControl`
- [Models](docs/models.md) — OpenAI Responses provider and an Anthropic
  Messages provider (thinking, tool use and prompt caching mapped onto the
  same canonical format); retry, fallback, and routing decorators; per-error-kind
  [error handlers](docs/running_agents.md) turn a failing run into a fallback
  completion; OpenAI stored prompts via `Agent.Prompt`
- [Middleware](docs/running_agents.md#middleware) — wrap a whole run: an
  evaluator loop, an approval policy, retry-the-run, structured logging,
  plan mode (read-only until an approved `submit_plan`), and an agent-kept
  todo list
- [MCP](docs/mcp.md) — a client for stdio and streamable-HTTP tool servers,
  and a server that exposes your own tools or a whole agent over MCP
- [Sandboxes](docs/sandbox.md) — run model-written code in Docker, SSH, or
  local backends; edit files via `apply_patch`; persistent shell sessions,
  and a command policy that filters what runs before the approval gate
- [Skills](docs/skills.md) — load `SKILL.md` Agent Skills
- [Tracing](docs/tracing.md) — spans for every model call, tool call, handoff,
  and guardrail; OpenTelemetry via the `tracing/otel` module
- [Logging](docs/logging.md) — structured `slog` records, off by default, with
  conversation content behind a second opt-in
- [Background tasks](docs/tasks.md) — sub-agents that outlive the turn that
  started them, waking the parent with their result
- [Testing](docs/testing.md) — `agentstest` scripts the model, so an agent's
  tools and decisions are covered offline

The full capability → API map is in the
[feature reference](docs/features.md).

## agents-server

A full-featured **[web app](cmd/agents-server/README.md)** that wraps the
SDK with a versioned REST API, WebSocket streaming, and an embedded browser UI.
Configure agents, MCP servers, sandboxes, guardrails, provider routes,
memories, and skills — then run conversations with streaming output, tool
approval, tracing, interactive sandbox terminals, and background tasks
(`spawn_task` subagents with durable state, approval bubbling, and automatic
completion wake-ups), all from the browser.

```bash
go run ./cmd/agents-server --port 9527
```

![agents-server screenshot](cmd/agents-server/screenshot.png)

## Examples

Nearly every capability has a runnable example under [examples/](examples/):

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello      # minimal agent
go run ./examples/handoffs   # triage agent → specialists
go run ./examples/hitl       # pause, approve, resume
```

See [docs/examples.md](docs/examples.md) for all of them — tools, streaming,
tracing, fallback, sessions, compaction, sandboxes, and more.

## License

[MIT](LICENSE)
