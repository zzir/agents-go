# agents-go docs

**Go agents. Local first.**

The documentation for both halves of the project: the **workbench** —
`agents-server`, the local agent workbench you run yourself — and the **SDK**
it is built on, the `agents` package, embeddable on its own. One core, two
consumers ([scope §1.1](explanation/scope.md#11-what-this-is)).

**New here?** [Running the workbench](tutorial/workbench.md) goes from a
binary to a first conversation with the Inspector open; nothing on that path
needs Docker. Embedding the SDK in your own program starts at the
[Quickstart](tutorial/quickstart.md).

The SDK's shape is idiomatic Go: generics instead of runtime reflection magic,
`context.Context` for cancellation, errors instead of exceptions, and
`iter.Seq2` for streaming — a run executes on the consumer's goroutine, so
abandoning a stream stops the run rather than leaking one. Behavior is
[specified](reference/spec.md), not inherited. Arriving from the OpenAI Agents
SDK for Python? Start at
[Differences from the Python SDK](explanation/migration_from_python.md).

---

## Documentation

The pages are sorted by what you came for.

### Tutorial — learn by doing

| Page | |
|---|---|
| [Running the workbench](tutorial/workbench.md) | **Start here.** From a binary to a first conversation with the Inspector open; a sandbox is the optional second chapter |
| [Quickstart](tutorial/quickstart.md) | The SDK: build and run your first agent in Go |
| [Examples](tutorial/examples.md) | Runnable SDK programs, one per capability |

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
| Workbench, power user | None of these are needed for the first conversation. [Deploying](howto/workbench-deploy.md) · [Authentication](howto/workbench-auth.md) · [Workflows](howto/workflows.md) · [Image input](howto/attachments.md) · [MCP OAuth troubleshooting](howto/mcp-oauth-troubleshooting.md) |

### Reference — look something up

| Page | |
|---|---|
| [Design spec](reference/spec.md) | The behavioral invariants — what is always true |
| [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go) | Every exported symbol, always in sync with the code |
| [The wire surface](reference/protocol.md) | What each workbench REST call means, and the WebSocket protocol |
| [Configuration](reference/configuration.md) | The workbench's flags, environment variables and runtime settings |

### Explanation — understand why

| Page | |
|---|---|
| [Architecture](explanation/architecture.md) | How the pieces compose, and where the extension points are |
| [Design decisions](explanation/decisions.md) | Settled decisions, each with the reason — read before reopening one |
| [Scope](explanation/scope.md) | What this is, what it deliberately is not, and the roadmap |
| [Differences from the Python SDK](explanation/migration_from_python.md) | For readers arriving from `openai-agents-python` |
| [Upstream watch](explanation/upstream_watch.md) | What was reviewed from the Python SDK, ported or declined |
| [Workbench design invariants](explanation/workbench-invariants.md) | The rules every workbench panel/handler pair follows |

### The workbench

[Manual](../cmd/agents-server/README.md) — flags, deployment, the REST and WebSocket surfaces, and the design invariants the panels obey.
