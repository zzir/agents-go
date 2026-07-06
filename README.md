# agents-go

A Go port of [openai-agents-python](https://github.com/openai/openai-agents-python)
(tracking **v0.17.7**). Build agents that call tools, hand off to one another,
enforce guardrails, stream events, persist sessions, pause for human approval,
and emit traces — all with idiomatic Go APIs.

**[Documentation](docs/index.md)** — mirrors the [Python SDK docs](https://openai.github.io/openai-agents-python/) structure, including a full [comparison with the Python SDK](docs/python_differences.md).

## Install

```bash
go get github.com/zzir/agents-go
```

Requires Go 1.26+. Optional backends (Docker/SSH sandbox, SQL sessions, skills)
are separate modules — see [Packages](#packages).

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

	res, err := agents.Run(context.Background(), agent, "Hello!", agents.RunOptions{
		ModelProvider: openai.NewProvider(), // reads OPENAI_API_KEY
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.FinalOutputString())
}
```

## Features

### Core

| Capability | API |
|---|---|
| Agents | `agents.Agent{...}` |
| Run (blocking) | `agents.Run(ctx, agent, input, opts)` |
| Streaming | `agents.RunStreamed(...)` → `Events()` iterator |
| Function tools | `agents.NewFunctionTool[Args, Result](name, desc, fn)` |
| Structured output | `agents.OutputType[T]()` |
| Dynamic output schema | `agents.NewDynamicOutputSchema(name, schema, strict)` (runtime JSON Schema) |
| Multimodal tool output | `agents.ToolOutputText/ToolOutputImage/ToolOutputFile` |
| Handoffs | `agents.HandoffTo(targetAgent)` |
| Agent as tool | `agent.AsTool(agents.AgentToolConfig{...})` |
| Guardrails | `InputGuardrails`, `OutputGuardrails`, tool-level guardrails (incl. pre-approval via `RunOptions.PreApprovalToolInputGuardrails`) |
| Human-in-the-loop | `tool.NeedsApproval`, `RunState.Approve/Reject`, `agents.ResumeRun` |
| SDK-only tool output metadata | `FunctionTool.CustomDataExtractor` → `ToolCallOutputItem.CustomData` (never sent to the model) |
| Instruction composition | `agents.WrapInstructions(inner, prefix, suffix)` |
| Composite hooks | `agents.CompositeRunHooks(hooks...)` |

### Sessions & state

| Capability | API |
|---|---|
| Sessions | `agents.Session`, `InMemorySession`, `memory.FileSession` (JSONL), `sessions` module (SQLite/Postgres) |
| Server-side sessions | `openai.ConversationsSession` (OpenAI Conversations API) |
| History compaction | `openai.CompactionSession` (server-side `responses.compact`), `agents.NewSlidingWindowSession` (local summarize) |
| Session forking | `agents.ForkSession` / `ForkSessionAt` / `IndexOfItemID` |
| Server-side state | `RunOptions.UsePreviousResponseID` / `RunOptions.ConversationID` |
| Stored prompts | `Agent.Prompt = agents.StaticPrompt(...)` / `agents.PromptFunc(...)` |

### Reliability & routing

| Capability | API |
|---|---|
| Retry | `agents.NewRetryModel(...)` / `agents.NewRetryProvider(...)` (backoff + jitter) |
| Fallback | `agents.NewFallbackModel(...)` / `agents.NewFallbackProvider(...)` (try backends in order) |
| Multi-provider routing | `agents.NewRouterProvider(...)` (per-agent backend by model-name prefix) |

### Integrations

| Capability | API |
|---|---|
| Tracing | `tracing.NewTracer`, `tracing.NewBatchProcessor`; `RunOptions.TraceGroupID/TraceMetadata` |
| MCP | `mcp.NewStdioServer / NewStreamableHTTPServer` (`NewSSEServer` deprecated) |
| Sandbox (code execution) | `sandbox.CodeTool` + Local / Docker / SSH backends |
| Web search | `bravesearch.New(bravesearch.Options{...})` (Brave Search API) |
| File editing | `sandbox.ApplyPatchTool` (Codex-style patches, edits through the sandbox) |
| Skills | `skills.Load / LoadRecursive / RenderIndex / ReadFileTool` (Agent Skills `SKILL.md`) |

## Tools

A function tool is a typed Go function. The argument struct is reflected into a
JSON schema (with strict-mode normalization) shown to the model:

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"the city"`
}

getWeather := agents.NewFunctionTool("get_weather", "Look up the weather.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "sunny in " + args.City, nil
	})

agent := &agents.Agent{Name: "bot", Model: "gpt-4o", Tools: []agents.Tool{getWeather}}
```

## Structured output

```go
type Sentiment struct {
	Label string `json:"label"`
	Score int    `json:"score"`
}

agent := &agents.Agent{Name: "classifier", Model: "gpt-4o", OutputType: agents.OutputType[Sentiment]()}
res, _ := agents.Run(ctx, agent, "I love this!", opts)
s, _ := agents.FinalOutputAs[Sentiment](res)
```

## Streaming

```go
sr := agents.RunStreamed(ctx, agent, "tell me a story", opts)
for event, err := range sr.Events() {
	if err != nil { panic(err) }
	if e, ok := event.(*agents.RunItemStreamEvent); ok {
		if msg, ok := e.Item.(*agents.MessageOutputItem); ok {
			fmt.Println(msg.Text())
		}
	}
}
res, _ := sr.FinalResult()
```

## Human-in-the-loop

```go
tool.NeedsApproval = true

res, _ := agents.Run(ctx, agent, "delete everything", opts)
for len(res.Interruptions) > 0 {
	for _, item := range res.Interruptions {
		res.State.Approve(item, false) // or res.State.Reject(item, false, "no")
	}
	res, _ = agents.ResumeRun(ctx, res.State, opts)
}
```

The paused state serializes to JSON (`res.State.MarshalJSON()`) and rebuilds with
`agents.RunStateFromJSON(data, registry)` for cross-process approval flows.

## Sessions

```go
sess, _ := memory.NewFileSession("sessions", "user-123") // sessions/user-123.jsonl
agents.Run(ctx, agent, "remember my name is Ada", agents.RunOptions{Session: sess, ModelProvider: p})
```

History is stored as JSONL with zero external dependencies. The `sessions`
module backs the same interface with SQLite/PostgreSQL, or implement
`agents.Session` yourself.

## Tracing

```go
exporter := tracing.NewConsoleExporter(os.Stdout)
proc := tracing.NewBatchProcessor(exporter, tracing.BatchProcessorOptions{})
defer proc.Shutdown(context.Background())

agents.Run(ctx, agent, "hi", agents.RunOptions{Tracer: tracing.NewTracer(proc), ModelProvider: p})
```

## MCP

```go
server, _ := mcp.NewStdioServer(ctx, "fs",
	exec.Command("npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"), mcp.Options{})
defer server.Close()

agent := &agents.Agent{Name: "a", Model: "gpt-4o", MCPServers: []agents.MCPServer{server}}
```

## Sandbox (code execution)

Run untrusted, agent-generated code in an isolated environment and expose it as a
tool. The Docker backend is a **separate module** so the core stays
dependency-light:

```go
import (
	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/docker" // go get github.com/zzir/agents-go/sandbox/docker
)

sb, _ := docker.New(docker.Options{Image: "python:3.12-slim",
	Limits: sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5}})
defer sb.Close()

agent := &agents.Agent{Name: "coder", Model: "gpt-4o",
	Tools: []agents.Tool{sandbox.CodeTool(sb, sandbox.CodeToolConfig{Name: "run_python"})}}
```

Backends:

- **Docker** — defaults to no network, read-only root fs, dropped capabilities,
  non-root user, and CPU/memory/PID/time limits.
- **Local** (`sandbox.NewLocal()`) — runs on the host **without isolation**; for
  trusted dev/tests only.
- **SSH** (`sandbox/ssh`, separate module) — runs commands on a remote host over
  SSH with files via SFTP; useful for a disposable VM, but provides **no**
  isolation or resource limits of its own.

## agents-server

A full-featured **[demo web app](cmd/agents-server/README.md)** that wraps the
SDK with a versioned REST API, WebSocket streaming, and an embedded browser UI.
Configure agents, MCP servers, sandboxes, memories, and skills — then run
conversations with streaming output, tool approval, and tracing, all from the
browser.

```bash
go run ./cmd/agents-server --port 8080
```

![agents-server screenshot](cmd/agents-server/screenshot.png)

## Examples

See [docs/examples.md](docs/examples.md) for what each one demonstrates.

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello
go run ./examples/tools
go run ./examples/handoffs
go run ./examples/streaming
go run ./examples/hitl
go run ./examples/tracing        # tracer → batch processor → console exporter
go run ./examples/fallback       # retry + fallback model decorators
go run ./examples/toolimage      # a tool returns a generated image to the model
go run ./examples/conversations  # server-side history via the Conversations API
go run ./examples/compaction     # auto-summarize long history via responses.compact
go run ./examples/slidingwindow  # local sliding-window history compaction
go run ./examples/sandbox        # writes & runs Python in a sandbox (host needs python3)
OPENAI_PROMPT_ID=pmpt_... go run ./examples/prompt        # drive an agent from a stored prompt
BRAVE_API_KEY=... go run ./examples/bravesearch           # web search tool

# examples in the optional submodules:
(cd sessions && go run ./example)   # SQLite-backed session
(cd skills && go run ./example)     # Agent Skills (SKILL.md)
(cd sandbox/docker && go run ./example)  # needs Docker
(cd sandbox/ssh && SSH_HOST=host SSH_USER=user SSH_KEY=~/.ssh/id_ed25519 \
	go run ./example)  # needs a reachable SSH host
```

## Packages

Core module path: `github.com/zzir/agents-go`.

| Package | What it is |
|---|---|
| `agents` | Core: agents, runner, tools, guardrails, sessions, HITL, tracing hooks |
| `models/openai` | OpenAI Responses API model provider (built on `openai-go` v3) |
| `memory` | `FileSession` (JSONL file store, zero dependencies) |
| `tracing` | Traces, spans, processors and exporters |
| `mcp` | Model Context Protocol client |
| `sandbox` | `Sandbox` interface + `CodeTool` + `apply_patch` + local backend |
| `tools/bravesearch` | Brave Search web-search tool |
| `sandbox/docker` | **separate module** — Docker sandbox backend |
| `sandbox/ssh` | **separate module** — remote SSH sandbox backend |
| `sessions` | **separate module** — SQLite/PostgreSQL session store (uptrace/bun) |
| `skills` | **separate module** — Agent Skills (`SKILL.md`) loader |
| `cmd/agents-server` | **separate module** — demo web app (REST + WS + UI) |

## Design notes

The core lives in a single `agents/` package. The original plan split it further
into `tools/`, `outputs/` and `models/`, but in Go those would form an import
cycle with the core (tool callbacks reference `RunContext`; the `Model` interface
references `Tool`), so they are kept together in `agents/`. Provider, storage,
tracing and MCP implementations live in subpackages that import `agents`. Items
use the `openai-go` Responses types as the wire format, mirroring how the Python
SDK reuses the OpenAI SDK types.

## License

MIT
