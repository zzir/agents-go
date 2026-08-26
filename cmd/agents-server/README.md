# agents-server — the agents-go workbench

**Go agents. Local first.** The Go-native agent workbench you run yourself,
built on the [agents-go](../../README.md) SDK: one binary, your data (SQLite
or PostgreSQL), an embedded Primer-styled UI. Configure agents, providers,
MCP servers, sandboxes, projects, guardrails, memories, skills, workflows and
triggers; run conversations with streaming output, tool approvals, traces, a
context lens, replay & fork, interactive sandbox terminals and background
tasks — solo or as a team.

![screenshot](screenshot.png)

## The manual

The workbench's documentation lives in [`docs/`](../../docs/) with the
rest of the project, sorted by what you came for:

| | |
|---|---|
| **Get it running** | [Running the workbench](../../docs/tutorial/workbench.md) — build, flags, first run |
| **Deploy it** | [Deploying the workbench](../../docs/howto/workbench-deploy.md) — deployment, logging, the database |
| **Let people in** | [Authentication](../../docs/howto/workbench-auth.md) — OAuth mode, ownership and roles, the audit log |
| **Call it** | [The wire surface](../../docs/reference/protocol.md) — what each REST call *means*, and the WebSocket protocol |
| **Endpoint schemas** | Generated, never hand-written: `/openapi.yaml`, browsable at `/docs` on a running server |
| **Change it** | [Design invariants](../../docs/explanation/workbench-invariants.md) — the rules every panel/handler pair follows |
| **Understand it** | [Architecture](../../docs/explanation/architecture.md#the-workbenchs-architecture) · [Scope and roadmap](../../docs/explanation/scope.md) |

The SDK underneath is documented separately: [spec](../../docs/reference/spec.md)
for its invariants, [design decisions](../../docs/explanation/decisions.md) for
why each is what it is.

## In this directory

- [`PROTOCOL.md`](PROTOCOL.md) — the shape the WebSocket protocol is moving
  *toward*, agreed up front. Forward-looking, and cited from the code that
  implements it; the protocol that ships today is in
  [the wire surface](../../docs/reference/protocol.md).
- `make openapi` regenerates `internal/docs/swagger.yaml` after any handler
  change. CI diffs it.
