# Deploying the workbench

Taking `agents-server` beyond `go run`. Start at
[Running the workbench](../tutorial/workbench.md) if you have not got it up yet.

### Requirements

Only sandboxes need anything beyond the binary: a Docker daemon — this
machine's, or a remote one over SSH or TCP — or any service speaking the E2B
API. The server shells out to no binary. Which daemon or service a sandbox
uses is its config, in
[Sandboxes](../reference/protocol.md#sandboxes--apiv1sandboxes).

### The container image

Published to GHCR and Docker Hub on each release:

```bash
docker run -p 9527:9527 -v agents-data:/data ghcr.io/zzir/agents-server:latest --host 0.0.0.0
```

Pass `--host 0.0.0.0` — the default `127.0.0.1` is unreachable from outside the
container. State persists in the `/data` volume (the default `data.db` lands
there) and the startup token is printed to the container logs. Any other flags
go on the same line; swap the image for `zzir/agents-server:latest` to pull from
Docker Hub. A sandbox of type `docker` inside the container still needs a
daemon to talk to — the host's socket mounted in, as
[`scripts/docker-compose.yml`](../../scripts/docker-compose.yml) does, or a
remote one over SSH or TCP.

### Deployment

Standing alone on localhost, no flags are needed. Behind a TLS-terminating
reverse proxy, two things change:

- **`--base-url` is required for OAuth flows** — MCP server OAuth, and
  `--auth oauth` refuses to start without it. Every externally visible URL is
  derived from it, never from `Forwarded` / `X-Forwarded-*`, which a direct
  client could forge. Only a bare `scheme://host[:port]` is accepted; the app
  assumes it is mounted at the proxy's root.
- **`--trusted-proxies` is required behind a proxy.** It names who may set
  `X-Forwarded-For`, and that header is where the client IP for the rate
  budgets and the access log comes from. Without it every request arrives
  from the proxy's address, so the whole team shares one per-IP budget — and
  the server warns at startup when `--base-url` is set without it. The
  default trusts no proxy: a direct client could otherwise put any address
  in the header and dodge the budgets. (This overrides gin's trust-everyone
  default.)

The server itself speaks plain HTTP; TLS is the proxy's job. Compression is
not: API responses go out gzip-compressed from 1 KiB when the client accepts
it, the UI's assets are pre-compressed at build, and the WebSocket stays
uncompressed by design — a proxy that compresses on its own gains nothing
there. Three per-IP
budgets exist, each answering `429` with code `rate_limited` when exceeded:

- **Credential guesses, 10/min.** A failed bearer on any authenticated route
  or on the WebSocket auth frame, a token login, a code exchange. A bearer
  that authenticates spends nothing — a signed-in client is never limited,
  however many tabs it opens — and an IP that has exhausted the budget is
  refused before its credential is checked.
- **OAuth flow steps, 60/min** (`oauth/*/start`, `oauth/*/callback`): they
  allocate server state per call but guess nothing.
- **Webhooks, 60/min** (`/hooks/:id`), burst 30.

`/auth/config` is a static fact and carries no budget.

### Logging

Structured records over [`log/slog`](https://pkg.go.dev/log/slog), to stderr.
`--log-format json` emits one JSON object per record for a collector; `text` is
the terminal default. A bad `--log-level` or `--log-format` is a start-up
error, not a silent fallback — a typo must not quietly turn logging down.

The SDK's own run-loop records join the same stream, so turns, tool calls,
handoffs and compaction show up beside the server's; most of them are `Debug`,
so it takes `--log-level debug` to see them. Whether they carry conversation
content is `log_sensitive_data` — stderr — which is a different switch from
`trace_include_sensitive_data`, the database one
([runtime settings](../reference/configuration.md#runtime-settings)).

---

## Database

SQLite in WAL mode with a 5s busy timeout by default — both applied as PRAGMA
statements and verified when the database opens, because the two SQLite drivers
the build can pick disagree on DSN pragma syntax and silently drop what they
don't recognize. Pass `--db` a `postgres://` (or `postgresql://`) DSN to use
PostgreSQL 16+ instead — same schema, created the same way; the Postgres pool
is capped at 16 connections:

```bash
./agents-server --db 'postgres://user:pass@localhost:5432/agents?sslmode=disable'
```

Run **one instance per database**. A single process holds the live truth about
running runs, cron schedules and OAuth in memory, and its startup sweep fails
every task left `working` by the last shutdown — so a second instance would
kill the first's work. On PostgreSQL a startup advisory lock refuses the second
instance outright — it lives on one long-held connection, so an
`idle_session_timeout` on the server would silently drop it; leave that off for
the workbench's role. On SQLite the single-file assumption stands. Horizontal
scaling is on the [roadmap](../explanation/scope.md), not shipped.

Every id that names one of our entities is a `uuid` column: UUIDv4 for
ordinary entities, UUIDv7 for the append-heavy `entries`, `trace_events` and
`audit_events`, whose ids double as the pagination cursors (`before_id` /
`before`). Order is read off an id only where nothing better exists: a
session's entries are read, paged, forked and compacted in `seq` order, while
trace events have no `seq` and list by id, which is append order within one
process and nothing more. An identifier of foreign shape stays text — entry
ids (`e<seq>`), span ids (`span_<hex>`), a model's `tool_call_id`. "Unset" is
NULL, never `""`. The token-mode local account has the fixed id
`00000000-0000-0000-0000-000000000001`. On PostgreSQL a malformed id in a
path answers 400 where SQLite stores any text, which is why CI runs the store
suite on PostgreSQL too (`AGENTS_PG_TEST_DSN`; see `scripts/ci.sh`).

Tables are created automatically on startup:

| Table               | Description                                                                         |
|---------------------|-------------------------------------------------------------------------------------|
| `sessions`          | Chat sessions; `owner_id` is the one ownership column (see [Ownership and roles](workbench-auth.md#ownership-and-roles)) |
| `entries`           | Session entries (the conversation, annotations and compaction checkpoints)          |
| `append_points`     | Where each session stands: branch tip + highest sequence number (see invariant 26)  |
| `agent_configs`     | Agent configurations                                                                |
| `mcp_servers`       | MCP server configurations                                                           |
| `skills`            | Stored `SKILL.md` documents (name/description denormalized from frontmatter)        |
| `memories`          | Agent memories                                                                      |
| `attachments`       | Image attachments: the bucket key and owner of each upload, and whether a message bound it |
| `settings`          | Global key-value settings                                                           |
| `sandboxes`         | Sandbox rows: where it runs (`docker` / `e2b`) and what runs on it                  |
| `projects`          | Per-user working trees on a sandbox (decisions §5.28); storage derived from the ids       |
| `guardrails`        | Custom guardrail definitions                                                        |
| `trace_events`      | Trace spans (agent, generation, function, handoff, compaction) + run lineage; payload referenced from `trace_blobs`; pruned in batches by `trace_retention_days` |
| `trace_blobs`       | Payload elements of a session's spans (input items, replies, tool definitions, instructions): one row per distinct element per session, gzip-compressed when smaller; dropped with the session's last event or by `trace_payload_retention_days` |
| `pending_approvals` | Runs paused for human-in-the-loop tool approval (persisted so they survive restart) |
| `tasks`             | Background tasks — sub-agents spawned via `spawn_task` and workflow executions (`kind`, `state`) — durable identity and status |
| `providers`         | Model-API endpoints and their credentials; agents reference one |
| `workflows`         | Fixed step sequences (each step: agent + prompt, with a stable id); an execution is a `tasks` row |
| `audit_events`      | Who did what, to what, when — see [Audit log](workbench-auth.md#audit-log)                          |
| `wakeups`           | "This session is owed a turn carrying this" — the debt background work leaves behind; settled rows are pruned after 7 days |
| `context_profiles`  | One row per session: what its last build put in front of the conversation (prompt layers, tool surface) |
| `users`             | Accounts and roles (see [Ownership and roles](workbench-auth.md#ownership-and-roles))                |
| `identities`        | OAuth identities linked to a user                                                   |
| `auth_tokens`       | Session tokens and personal access tokens (hashes only)                             |
| `triggers`          | Cron and webhook starts filed on a session (see [Workflows](../reference/protocol.md#workflows--apiv1workflows)) |

The database file can be deleted and recreated freely — there is no migration
mechanism.
