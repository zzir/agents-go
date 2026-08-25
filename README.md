<div align="center">

# agents-go workbench

**Go agents. Local first.**

The Go-native agent workbench you run yourself. Run agents and workflows in a
sandbox, behind tool approvals; debug with traces, replay & fork. One binary,
your data, embeddable SDK. Solo or as a team.

[![Release](https://img.shields.io/github/v/release/zzir/agents-go)](https://github.com/zzir/agents-go/releases)
[![CI](https://github.com/zzir/agents-go/actions/workflows/ci.yml/badge.svg)](https://github.com/zzir/agents-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzir/agents-go.svg)](https://pkg.go.dev/github.com/zzir/agents-go)
[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Get started](#get-started) ·
[What you get](#what-you-get) ·
[Workbench manual](cmd/agents-server/README.md) ·
[SDK docs](https://zzir.github.io/agents-go/) ·
[Examples](docs/examples.md)

</div>

![agents-go workbench — a conversation with the trace panel open and a sandbox terminal tab](cmd/agents-server/screenshot.png)

## Get started

1. **Get the binary.** Download the archive for your OS and CPU from
   [Releases](https://github.com/zzir/agents-go/releases) and extract it — the
   `agents-server` binary is at the top level. (On macOS, Gatekeeper refuses
   the unsigned binary; `xattr -d com.apple.quarantine ./agents-server`
   clears it.) Or build from source: see the manual's
   [Quick start](cmd/agents-server/README.md#quick-start).

2. **Run it.**

   ```bash
   ./agents-server
   ```

   It listens on `http://127.0.0.1:9527` and prints an auth token at startup;
   paste the token into the login screen. A Docker daemon is required for
   sandboxes — on this machine, or a remote daemon (SSH or TCP). State lives
   in `data.db` in the directory you ran it from (`--db`; a `postgres://` DSN
   uses PostgreSQL instead); with a local daemon, project trees live under
   `--workspace` (default `.`). All flags:
   [manual](cmd/agents-server/README.md#flags).

3. **Add a provider, create an agent, chat.** Settings → Providers: an OpenAI
   or Anthropic API key, or sign in with ChatGPT. Settings → Agents: name,
   model, instructions, tools. New Chat.

## What you get

- **Zero infrastructure.** One process and one database — a single SQLite
  file by default, or your own PostgreSQL — hold the agents, sessions,
  traces, approvals and tasks. The one external dependency is the
  Docker daemon that backs sandboxes — the server itself shells out to
  nothing (details in the [server manual](cmd/agents-server/README.md)).
- **The transcript is the truth.** A session is an append-only tree: every
  turn is persisted as it completes, so a cancelled or failed run keeps what
  finished and a paused run survives a restart. Regenerate a turn, or fork
  any turn into a new session; branches stay visible and switchable.
- **Context lens.** What the model actually receives — instructions, global
  system prompt, memories, skills index, tools by source, conversation — with
  the token cost of each, how much of the window is in use, cache hits,
  growth per call, and how far to the next auto-compaction. Compact on demand.
- **Traces without a backend.** Agent / generation / tool / sandbox / handoff /
  guardrail / compaction spans with tokens and latency, in a panel beside the
  conversation. No collector to run.
- **Replay any generation.** Re-run a traced model call with a different
  prompt, model, settings or tools — streaming, with a diff against the
  original and the attempts kept side by side. No session is touched.
- **Real sandboxes behind an approval gate.** Docker containers — on this
  machine, or a remote daemon (SSH or TCP); the model
  reads and edits files (`apply_patch`) and runs commands; approve a command
  once, trust that command, or trust the session; interactive terminals into a
  container, in the browser.
- **Background tasks, workflows, triggers.** `spawn_task` sub-agents that
  outlive the turn and wake the parent when done (a failed one resumes where it
  stopped); fixed step sequences as workflows, started by the model, by hand,
  by cron, or by a signed webhook — and, for an agent you opt in, authored
  from the chat, each save reviewed and approved in the conversation.
- **The configuration surface.** MCP servers (streamable HTTP, with OAuth),
  Agent Skills, sandboxes, projects, memories, guardrails.
- **Providers.** OpenAI Responses API (API key or ChatGPT sign-in), Anthropic
  Messages API, or any Responses-compatible endpoint by base URL.
- **A team, when you need one.** `--auth oauth` replaces the single token with
  Google sign-in and an allowlist: each person's sessions are their own,
  agents, providers, MCP servers, skills and workflows are per person with
  admin-published global rows, host configuration is admin-written, every
  change lands in an audit log, personal access tokens serve scripts, and
  stored credentials are sealed at rest with a key only the process holds.
  Details: [Authentication](cmd/agents-server/README.md#authentication).

Together these close the debug loop: see what the model saw, change it,
re-run — one call, one turn, or a fork — and diff the results.

## Built on agents-go, a Go SDK you can embed

The workbench is a Go program on top of the `agents` package. Anything it does
you can do from your own code:

```bash
go get github.com/zzir/agents-go
```

> **Pre-1.0 API notice.** Until v1.0.0 a minor release may rename or remove
> exported identifiers — pin the version. Breaking renames are batched, and the
> [release notes](https://github.com/zzir/agents-go/releases) carry every old
> spelling beside the new.

```go
package main

import (
	"context"
	"fmt"

	"github.com/zzir/agents-go/agents"
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

The [Quickstart](docs/quickstart.md) continues from here — handoffs,
guardrails, typed tools, structured output, streaming, approvals. By topic:

- [Tools](docs/tools.md) — typed function tools (the argument struct becomes
  the JSON schema), agents-as-tools, multimodal output, per-tool approval
- [Handoffs](docs/handoffs.md), [Guardrails](docs/guardrails.md),
  [Human-in-the-loop](docs/human_in_the_loop.md) — a paused run serializes to
  JSON and resumes in another process
- [Sessions](docs/sessions.md) — append-only entries, branching, crash
  recovery, [compaction](docs/sessions.md#run-level-compaction); in-memory,
  JSONL, SQLite/Postgres or OpenAI server-side
- [Streaming](docs/streaming.md) — a run is a range-able iterator; steer it or
  queue follow-ups mid-run
- [Models](docs/models.md) — OpenAI Responses and Anthropic Messages
  providers; retry, fallback and routing decorators;
  [middleware](docs/running_agents.md#middleware) around a whole run
- [MCP](docs/mcp.md), [Sandboxes](docs/sandbox.md), [Skills](docs/skills.md),
  [Tracing](docs/tracing.md), [Background tasks](docs/tasks.md),
  [Testing](docs/testing.md) — a scripted `Model` fake tests agents
  without a key

The core is one small module; the heavier capabilities are opt-in submodules
([packages](docs/features.md#packages)). Arriving from the OpenAI Agents SDK?
Start at [Coming from Python?](docs/migration_from_python.md).

The SDK stands alone — it never depends on or reports to the workbench
([spec §1.2](docs/spec.md#12-non-goals)).

Nearly every SDK capability has a runnable example under
[examples/](examples/):

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello      # minimal agent
go run ./examples/handoffs   # triage agent → specialists
go run ./examples/hitl       # pause, approve, resume
```

## License

[MIT](LICENSE)
