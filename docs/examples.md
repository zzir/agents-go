# Examples

Runnable examples live in [`examples/`](../examples). Each is a standalone `main` package:

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello
```

| Example | Shows |
|---|---|
| [`examples/hello`](../examples/hello/main.go) | The minimal agent: provider, instructions, one `Run` |
| [`examples/tools`](../examples/tools/main.go) | Typed function tools via `NewFunctionTool` |
| [`examples/toolimage`](../examples/toolimage/main.go) | A tool returning image content (`ToolOutputImage`) |
| [`examples/handoffs`](../examples/handoffs/main.go) | A triage agent delegating to specialists with `HandoffTo` |
| [`examples/streaming`](../examples/streaming/main.go) | Consuming `RunStreamed` events with `range` |
| [`examples/hitl`](../examples/hitl/main.go) | Human-in-the-loop: interrupt, approve/reject, resume |
| [`examples/errorhandlers`](../examples/errorhandlers/main.go) | `RunOptions.ErrorHandlers`: fallback final outputs for max-turns and invalid structured output |
| [`examples/tracing`](../examples/tracing/main.go) | The tracing pipeline: tracer → batch processor → console exporter, plus `TraceGroupID`/`TraceMetadata` |
| [`examples/fallback`](../examples/fallback/main.go) | Retry + fallback model decorators, with `WithShouldFallback` classification |
| [`examples/compaction`](../examples/compaction/main.go) | `openai.CompactionSession`: server-side history compaction via `responses.compact` |
| [`examples/slidingwindow`](../examples/slidingwindow/main.go) | `SlidingWindowSession`: provider-agnostic history summarization with pair-safe splits |
| [`examples/conversations`](../examples/conversations/main.go) | `openai.ConversationsSession`: history stored server-side via the Conversations API |
| [`examples/prompt`](../examples/prompt/main.go) | Binding an OpenAI stored prompt via `Agent.Prompt` |
| [`examples/bravesearch`](../examples/bravesearch/main.go) | The Brave web-search tool (`tools/bravesearch`) |
| [`examples/sandbox`](../examples/sandbox/main.go) | An agent that writes and runs code in a local sandbox |
| [`sandbox/docker/example`](../sandbox/docker/example/main.go) | The Docker sandbox backend (separate module) |
| [`sandbox/ssh/example`](../sandbox/ssh/example/main.go) | The SSH sandbox backend (separate module) |
| [`sessions/example`](../sessions/example/main.go) | SQLite-backed session persistence (separate module) |
| [`skills/example`](../skills/example/main.go) | Loading `SKILL.md` skills into an agent (separate module) |

The test suites are also worth reading as usage references — `agents/run_test.go` shows how to script a fake model (`Agent.ModelImpl`) for offline tests of your own agents.
