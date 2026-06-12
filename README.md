# agents-go

A Go port of [openai-agents-python](https://github.com/openai/openai-agents-python)
(tracking **v0.17.4**). Build agents that call tools, hand off to one another,
enforce guardrails, stream events, persist sessions, pause for human approval,
and emit traces — all with idiomatic Go APIs.

## Install

```bash
go get github.com/zzir/agents-go
```

Requires Go 1.26+.

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

| Capability | API |
|---|---|
| Agents | `agents.Agent{...}` |
| Run (blocking) | `agents.Run(ctx, agent, input, opts)` |
| Streaming | `agents.RunStreamed(...)` → `Events()` iterator |
| Function tools | `agents.NewFunctionTool[Args, Result](name, desc, fn)` |
| Structured output | `agents.OutputType[T]()` |
| Handoffs | `agents.HandoffTo(targetAgent)` |
| Agent as tool | `agent.AsTool(agents.AgentToolConfig{...})` |
| Guardrails | `InputGuardrails`, `OutputGuardrails`, tool-level guardrails |
| Sessions | `agents.Session`, `agents.InMemorySession`, `memory.FileSession` |
| Human-in-the-loop | `tool.NeedsApproval`, `RunState.Approve/Reject`, `agents.ResumeRun` |
| Tracing | `tracing.NewTracer`, `tracing.NewBatchProcessor` |
| MCP | `mcp.NewStdioServer / NewStreamableHTTPServer / NewSSEServer` |

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

History is stored as JSONL with zero external dependencies. Implement the
`agents.Session` interface yourself to back it with a database instead.

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
tool. Backends (Docker, Kubernetes Jobs) are **separate modules** so the core
stays dependency-light — install only the one you use:

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

Both backends default to: no network, read-only root fs, dropped capabilities,
non-root user, and CPU/memory/PID/time limits. The Kubernetes backend
(`.../sandbox/k8s`) additionally runs each call as a one-shot Job with no service
account token. `sandbox.NewLocal()` runs on the host **without isolation** — for
trusted dev/tests only.

## Packages

Core module path: `github.com/zzir/agents-go`.

- `.../agents` — core: agents, runner, tools, guardrails, sessions, HITL, tracing hooks.
- `.../models/openai` — OpenAI Responses API model provider (built on `openai-go` v3).
- `.../memory` — `FileSession` (JSONL file store, zero dependencies).
- `.../tracing` — traces, spans, processors and exporters.
- `.../mcp` — Model Context Protocol client.
- `.../sandbox` — `Sandbox` interface + `CodeTool` + local backend.
- `.../sandbox/docker`, `.../sandbox/k8s` — **separate modules** with the Docker / Kubernetes backends.
- `.../examples` — runnable examples (`hello`, `tools`, `handoffs`, `streaming`, `hitl`, `sandbox`).

## Examples

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello
go run ./examples/tools
go run ./examples/handoffs
go run ./examples/streaming
go run ./examples/hitl
go run ./examples/sandbox   # writes & runs Python in a sandbox (host needs python3)

# isolated backends live in their own modules:
(cd sandbox/docker && OPENAI_API_KEY=$OPENAI_API_KEY go run ./example)  # needs Docker
(cd sandbox/k8s    && OPENAI_API_KEY=$OPENAI_API_KEY go run ./example)  # needs a cluster
```

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
