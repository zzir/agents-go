# Examples

Runnable examples live in [`examples/`](../examples). Each is a standalone `main` package:

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/hello
```

| Example | Shows |
|---|---|
| [`examples/hello`](../examples/hello/main.go) | The minimal agent: provider, instructions, one `Run` |
| [`examples/testing`](../examples/testing/agent_test.go) | Testing an agent with `agentstest`: scripted model, no API key ([Testing](testing.md)) |
| [`examples/tools`](../examples/tools/main.go) | Typed function tools via `NewFunctionTool` |
| [`examples/toolimage`](../examples/toolimage/main.go) | A tool returning image content (`ToolOutputImage`) |
| [`examples/handoffs`](../examples/handoffs/main.go) | A triage agent delegating to specialists with `HandoffTo` |
| [`examples/streaming`](../examples/streaming/main.go) | Ranging a `RunStream` |
| [`examples/hitl`](../examples/hitl/main.go) | Human-in-the-loop: interrupt, approve/reject, resume |
| [`examples/errorhandlers`](../examples/errorhandlers/main.go) | `RunOptions.Exec.ErrorHandlers`: fallback final outputs for max-turns and invalid structured output |
| [`examples/tracing`](../examples/tracing/main.go) | The tracing pipeline: tracer → batch processor → console exporter, plus `TraceGroupID`/`TraceMetadata` |
| [`examples/fallback`](../examples/fallback/main.go) | Retry + fallback model decorators, with `WithShouldFallback` classification |
| [`examples/anthropic`](../examples/anthropic/main.go) | Streaming an agent on Claude via the Anthropic Messages provider — tool loop plus token deltas (separate module) |
| [`examples/compaction`](../examples/compaction/main.go) | `openai.CompactionSession`: server-side history compaction via `responses.compact` |
| [`examples/toolstream`](../examples/toolstream/main.go) | `ToolContext.Emit`: a running tool's progress on the stream, and why it is not the answer |
| [`examples/steering`](../examples/steering/main.go) | `RunControl`'s three queues: steer, next-turn, follow-up |
| [`examples/branching`](../examples/branching/main.go) | A session is a tree: branch from an earlier point without deleting the attempt |
| [`examples/projector`](../examples/projector/main.go) | `EntryProjector`: deciding what the model gets to read |
| [`examples/mcpserver`](../examples/mcpserver/main.go) | Serving SDK tools over MCP to an editor or another agent |
| [`examples/tasks`](../examples/tasks/main.go) | Background sub-agents: spawn, the wake-up debt, and the parent woken with the result |
| [`examples/middleware`](../examples/middleware/main.go) | Run middleware: `Retry` + `Approval` policy + evaluator-driven `Loop`, stacked |
| [`examples/planmode`](../examples/planmode/main.go) | Plan mode + todo list: read-only exploration, a `submit_plan` approval pause, then execution in the same run |
| [`examples/runcompaction`](../examples/runcompaction/main.go) | Run-level compaction: a `compaction.Strategy` folding tool results mid-run, at the turn boundary |
| [`examples/conversations`](../examples/conversations/main.go) | `openai.ConversationsSession`: history stored server-side via the Conversations API |
| [`examples/prompt`](../examples/prompt/main.go) | Binding an OpenAI stored prompt via `Agent.Prompt` |
| [`examples/bravesearch`](../examples/bravesearch/main.go) | The Brave web-search tool (`tools/bravesearch`) |
| [`examples/sandbox`](../examples/sandbox/main.go) | An agent that writes and runs code in a local sandbox |
| [`sandbox/docker/example`](../sandbox/docker/example/main.go) | The Docker sandbox backend (separate module) |
| [`sandbox/ssh/example`](../sandbox/ssh/example/main.go) | The SSH sandbox backend (separate module) |
| [`sessions/example`](../sessions/example/main.go) | SQLite-backed session persistence (separate module) |
| [`skills/example`](../skills/example/main.go) | Loading `SKILL.md` skills into an agent (separate module) |

## Extra setup

Most examples only need `OPENAI_API_KEY`. The exceptions:

- `examples/prompt` — a stored prompt ID: `OPENAI_PROMPT_ID=pmpt_... go run ./examples/prompt`
- `examples/anthropic` — `ANTHROPIC_API_KEY=... go run .` (from its directory; separate module)
- `examples/bravesearch` — `BRAVE_API_KEY=... go run ./examples/bravesearch`
- `examples/sandbox` — the host needs `python3`
- Examples in the optional submodules run from their module directory:

```bash
(cd sessions && go run ./example)        # SQLite-backed session
(cd skills && go run ./example)          # Agent Skills (SKILL.md)
(cd sandbox/docker && go run ./example)  # needs a running Docker daemon
(cd sandbox/ssh && SSH_HOST=host SSH_USER=user SSH_KEY=~/.ssh/id_ed25519 \
	go run ./example)                    # needs a reachable SSH host
```

The test suites are also worth reading as usage references — `agents/run_test.go` shows how to script a fake model (`Agent.ModelImpl`) for offline tests of your own agents.
