# Deploying the workbench

Taking `agents-server` beyond `go run`. Start at
[Running the workbench](../tutorial/workbench.md) if you have not got it up yet.

### Requirements

A Docker daemon — this machine's, or a remote one over SSH or TCP — is the
server's one external dependency, and only sandboxes need it; the server
shells out to no binary. Which daemon a sandbox uses is its `config.host`, in
[Sandbox targets](../reference/protocol.md#sandbox-targets--apiv1sandbox-targets).

### Deployment

Standing alone on localhost, no flags are needed. Behind a TLS-terminating
reverse proxy, two things change:

- **`--base-url` is required for OAuth flows** — MCP server OAuth, and
  `--auth oauth` refuses to start without it. Every
  externally visible URL — an OAuth `redirect_uri` must match what the browser
  loaded — is derived from it. Forwarding headers (`Forwarded`,
  `X-Forwarded-*`) are deliberately never consulted for URL construction: a
  direct client can forge them, and an explicit origin cannot be spoofed. Only
  a bare `scheme://host[:port]` is accepted; the app assumes it is mounted at
  the proxy's root.
- **`--trusted-proxies` is required behind a proxy.** It names who may set
  `X-Forwarded-For`, and that header is where the client IP for the rate
  budgets and the access log comes from. Without it every request arrives
  from the proxy's address, so the whole team shares one per-IP budget — and
  the server warns at startup when `--base-url` is set without it. The
  default trusts no proxy: a direct client could otherwise put any address
  in the header and dodge the budgets. (This overrides gin's trust-everyone
  default.)

The server itself speaks plain HTTP; TLS is the proxy's job. Three per-IP
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

`slog.Handler` is the swap point, so there is no logger interface here to
replace: pointing records somewhere else is a different handler in
`internal/logging`, and nothing else moves. Subsystems reach the logger through
`logging.Ctx(ctx)`; a context nobody wired yields one that discards, so no call
site checks and nothing writes anywhere unasked.

The SDK's own run-loop records join the same stream — the run's logger is
handed to `agents.LogConfig`, so turns, tool calls, handoffs and compaction
show up beside the server's. Most of what the run loop says is `Debug`, so it
takes `--log-level debug` to see. Whether those records carry conversation
content is the `log_sensitive_data` setting, which is NOT
`trace_include_sensitive_data`: one puts content in the database, the other in
stderr, and each has to be decided on purpose.

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

Every id column — primary keys and the foreign keys that reference them — is
typed `uuid`; 16 bytes a key on PostgreSQL. Ordinary entities hold a UUIDv4
(`store.NewID`) — their row counts never make index locality matter. The
APPEND-HEAVY tables — `entries`, `trace_events`, `audit_events` — hold a
UUIDv7 (`store.NewTimeID`): time-ordered, so inserts land at the right edge
of an index and rows created together sit together as those tables grow into
the millions; their ids are also the pagination cursors (`before_id` /
`before`), which read order off the id. Elsewhere order
is never read off the id where it matters: a session's entries are read,
paged, forked and compacted in `seq` order — the append position the SDK
assigns, which a clock stepping backwards or a second process cannot
reorder — and an entry cursor names a row, whose position is then its
`seq`. Trace events have no `seq` and list by id, which is append order
within one process (Go's `uuid.NewV7` is monotonic there) and nothing more.
One rule: an id that names one of OUR entities is a uuid; an identifier of
foreign shape stays text — entry ids (`e<seq>`, by sequence), span ids
(`span_<hex>`, the OTel width), a model's `tool_call_id`, an audit line's
`resource`. "Unset" is NULL, never `""` (`nullzero`). The
token-mode local account has the fixed id
`00000000-0000-0000-0000-000000000001`. On PostgreSQL a malformed id in a path
answers 400; SQLite stores any text — which is why CI runs the store suite on
PostgreSQL as well (`AGENTS_PG_TEST_DSN`; see `scripts/ci.sh`): a raw-SQL
write that binds `""` where a uuid column expects NULL passes SQLite and fails
only there. Secrets — session tokens, PATs, OAuth state, the webhook secret —
are not ids and stay 256-bit `crypto/rand`; a UUID's 122 random bits would be
a downgrade.

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
| `settings`          | Global key-value settings                                                           |
| `sandbox_configs`   | Sandbox configurations                                                              |
| `projects`          | Per-user working trees on a sandbox (decisions §5.28); storage derived from the ids       |
| `guardrails`        | Custom guardrail definitions                                                        |
| `trace_events`      | Trace spans (agent, generation, function, handoff, compaction) + run lineage; pruned in batches by `trace_retention_days` |
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
