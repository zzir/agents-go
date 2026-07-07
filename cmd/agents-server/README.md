# agents-server

A full-featured agent platform built on the [agents-go](../../README.md) SDK.
Single binary, embedded SPA, SQLite — deploy and run in seconds. Provides a REST
API, WebSocket streaming, and a Primer-styled web UI for configuring agents, MCP
servers, sandboxes, guardrails, memories, and skills.

![screenshot](screenshot.png)

## Contents

- [Quick start](#quick-start) — build, flags
- [Authentication](#authentication)
- [REST API](#rest-api) — [errors](#errors) · [conventions](#response-conventions) · [sessions](#sessions--apiv1sessions) · [runs / SSE](#runs--apiv1runs) · [approvals](#approvals--apiv1approvals) · [agents](#agents--apiv1agents) · [MCP servers](#mcp-servers--apiv1mcp-servers) · [memories](#memories--apiv1memories) · [settings](#settings--apiv1settings) · [skills](#skills--apiv1skills-read-only) · [skill repos](#skill-repos--apiv1skill-repos) · [provider routes](#provider-routes--apiv1provider-routes) · [guardrails](#guardrails--apiv1guardrails) · [sandboxes](#sandboxes--apiv1sandboxes) · [playground](#playground--apiv1playground) · [secret handling](#secret-handling) · [OpenAPI](#openapi)
- [WebSocket protocol](#websocket-protocol)
- [Architecture](#architecture)
- [Database](#database)
- [Roadmap](#roadmap)

## Quick start

```bash
go build -o agents-server ./cmd/agents-server
./agents-server --port 9527
```

On startup the server prints an auto-generated auth token. Open
`http://127.0.0.1:9527` to access the web UI.

### Flags

| Flag                    | Default   | Description                                        |
|-------------------------|-----------|----------------------------------------------------|
| `--port`                | `9527`    | HTTP listen port                                   |
| `--db`                  | `data.db` | SQLite database file                               |
| `--workspace`           | `.`       | Workspace directory for skills and file operations |
| `--token`               | auto      | Auth token; randomly generated when omitted        |
| `--allow-local-sandbox` | `false`   | Allow creating local (non-isolated) sandboxes      |

## Authentication

REST requests authenticate with a Bearer token in the `Authorization` header:

- `Authorization: Bearer <token>`

The `?token=<token>` query parameter is no longer accepted for REST — it leaked
into browser history and proxy logs. The WebSocket instead authenticates at the
application level, via its first message (see [WebSocket protocol](#websocket-protocol)).

Exempt from auth: the browser-facing OAuth callbacks, the OpenAPI document
(`GET /api/v1/openapi.yaml`), the `/api/v1/auth/*` login/check endpoints, and
`GET /health`.

## REST API

Base path `/api/v1`. The legacy `/api` prefix still resolves as a **deprecated
alias** kept for one release — migrate to `/api/v1`. All request and response
bodies are JSON.

### Errors

Every non-2xx response uses a single error envelope:

```json
{
  "error": {
    "code": "not_found",
    "message": "not found"
  }
}
```

`code` is a stable, machine-readable identifier; `message` is human-readable
detail.

| Code           | HTTP | Meaning                                                                       |
|----------------|------|-------------------------------------------------------------------------------|
| `validation`   | 400  | Malformed request body or invalid parameter                                   |
| `unauthorized` | 401  | Missing or invalid Bearer token                                               |
| `forbidden`    | 403  | Operation disabled by server policy                                           |
| `not_found`    | 404  | No such resource                                                              |
| `conflict`     | 409  | Resource is in the wrong state for the request                                |
| `upstream`     | 502  | A failing upstream dependency (model provider, MCP server, sandbox host, git) |
| `internal`     | 500  | Unexpected server error (detail is logged, not returned)                      |

### Response conventions

- **Create** returns `201 Created` with the created resource.
- **Update** (`PUT`/`PATCH`) returns `200 OK` with the full updated resource.
- **Delete** returns `204 No Content`.
- A write (update or delete) against a missing resource returns `404`.
- Secret fields are write-only — see [Secret handling](#secret-handling).

### Sessions — `/api/v1/sessions`

| Method | Path                      | Description                                                          |
|--------|---------------------------|----------------------------------------------------------------------|
| GET    | `/sessions`               | List sessions                                                        |
| POST   | `/sessions`               | Create session (`{name?, agent_config_id?}`)                         |
| GET    | `/sessions/:id`           | Get session                                                          |
| PATCH  | `/sessions/:id`           | Partial update — `{name?, pinned?}`, returns the updated session     |
| DELETE | `/sessions/:id`           | Delete session and its messages and traces                           |
| GET    | `/sessions/:id/messages`  | List conversation messages (paginated)                               |
| GET    | `/sessions/:id/traces`    | List trace events (paginated)                                        |
| POST   | `/sessions/:id/fork`      | Fork session                                                         |
| POST   | `/sessions/:id/runs`      | Start a run on the session (see [Runs](#runs--apiv1runs))            |
| GET    | `/sessions/:id/approvals` | List pending approvals (see [Approvals](#approvals--apiv1approvals)) |

`POST /sessions` accepts an optional `agent_config_id` to bind the session to an
agent at creation (it must reference an existing agent). Rename and pin are a
single `PATCH /sessions/:id` accepting a partial `{name?, pinned?}` body; both
the separate `PUT` rename and `PATCH /sessions/:id/pin` endpoints are gone.

`fork` copies the source session's messages (and their traces) into a new
session. Its body is optional: `{message_id?, exclusive?, label?}`. Omit
`message_id` to fork everything; supply it to bound the copy up to and including
that message (`exclusive: true` excludes the boundary message itself). The
session inherits the source's `agent_config_id`.

**Pagination** — `messages` and `traces` accept optional `?limit=` and
`?before_id=`. Without `limit` the full list is returned (oldest-first),
backward-compatible with older clients. With `limit`, the newest `limit` items
are returned; page backwards by passing the smallest id you received as
`before_id` (an exclusive upper bound).

### Runs — `/api/v1/runs`

Runs are the REST surface for starting and observing agent executions. They share
the same run hub as the WebSocket transport, so a run started over either
transport is observable over both. Crucially, **runs execute server-side,
independent of the connection that started them** — a dropped client or a page
reload does NOT cancel the run. Reconnect and resubscribe (via
`GET /runs/:id/events` or the WebSocket `run.subscribe`) to pick the stream back
up without loss.

| Method | Path                 | Description                                            |
|--------|----------------------|--------------------------------------------------------|
| POST   | `/sessions/:id/runs` | Start a run — `{input, agent_config_id?, sandbox_id?}` |
| GET    | `/runs/:id`          | Get run status                                         |
| GET    | `/runs/:id/events`   | Stream run events (Server-Sent Events)                 |
| POST   | `/runs/:id/cancel`   | Cancel the run — `204`                                 |

`POST /sessions/:id/runs` returns `201` with `{run_id, session_id, status}`. With
`?wait=true` it blocks until the run ends and returns `200` with
`{run_id, session_id, status, final_output}` — or, when the run pauses for tool
approval, `{run_id, session_id, status: "interrupted"}` (list
`/sessions/:id/approvals` and decide; the decision resumes execution under a new
run id). It returns `409` if the session already has an active run.

`GET /runs/:id` returns `{run_id, session_id, status, last_seq, agent_config_id?,
sandbox_id?}`. `status` is one of `running`, `interrupted`, `completed`, `error`,
or `cancelled`. Finished runs stay queryable and replayable for **15 minutes**
after they end, then `GET /runs/:id` returns 404 (the conversation itself is
always in `/sessions/:id/messages`).

`GET /runs/:id/events` is a Server-Sent Events stream. (This is plain HTTP SSE
for API consumers — unrelated to MCP's deprecated SSE transport, which this
server does not expose.) Each event's `id:` is the hub sequence number;
reconnect with the `Last-Event-ID` header (or `?from_seq=`) to resume without
losing events. The stream closes after a terminal event: `run.output`,
`run.error`, `run.cancelled`, or `run.interrupted` (paused for approval — after
approving/rejecting, reconnect to the SAME run id with your `Last-Event-ID`
— the resumed events continue its sequence).
Event payloads mirror the WebSocket [server→client events](#server--client).

Start a run and stream it with plain curl (token from server startup):

```bash
TOKEN=...; H="Authorization: Bearer $TOKEN"; BASE=http://127.0.0.1:9527/api/v1
SID=$(curl -s -H "$H" -X POST $BASE/sessions -d '{"name":"cli"}' | jq -r .id)
RUN=$(curl -s -H "$H" -X POST $BASE/sessions/$SID/runs \
      -d '{"input":"hello","agent_config_id":"<agent-id>"}' | jq -r .run_id)
curl -N -H "$H" $BASE/runs/$RUN/events          # stream until run.output

# or fire-and-wait in one call:
curl -s -H "$H" -X POST "$BASE/sessions/$SID/runs?wait=true" \
     -d '{"input":"hello","agent_config_id":"<agent-id>"}' | jq .final_output
```

### Approvals — `/api/v1/approvals`

Human-in-the-loop tool approvals. When a tool requires approval the run pauses;
the pending decision is **persisted to the database, so it survives a server
restart** and is addressable over REST.

| Method | Path                               | Description                                                          |
|--------|------------------------------------|----------------------------------------------------------------------|
| GET    | `/sessions/:id/approvals`          | List pending tool-call approvals for the session                     |
| POST   | `/approvals/:tool_call_id/approve` | Approve — body `{scope?}`, resumes the run, `202` `{run_id, status}` |
| POST   | `/approvals/:tool_call_id/reject`  | Reject — body `{reason?}`, resumes the run, `202` `{run_id, status}` |

Approve/reject resume the run through the shared hub, so the resulting events
stream over `GET /runs/:id/events` or the WebSocket. A decision on a session that
already has an active run returns `409`.

**exec_command session approval.** An agent whose `approve_tools` includes
`exec_command` gates each shell command through a per-session trust store instead
of approving every call. The approval surfaces the command itself; the approve
body `scope` decides how far it extends: `once` (default — just this call),
`same` (trust this exact command for the rest of the session), or `all` (trust
every command this session). Trust is in-memory and per session: it survives
interrupt/resume and resets on restart. The WebSocket `tool.approve` message
carries the same `scope` field. Matching is exact, so approving `go test` never
green-lights `go test && rm -rf`.

Unanswered approvals expire after the `approval_ttl_minutes` setting (default
`1440` = 24h; `0` disables expiry). On timeout the pending record is dropped and
an error annotation is written to the session so the timeout is visible rather
than silently vanishing.

### Agents — `/api/v1/agents`

| Method | Path                         | Description                              |
|--------|------------------------------|------------------------------------------|
| GET    | `/agents`                    | List agent configs                       |
| POST   | `/agents`                    | Create agent                             |
| GET    | `/agents/:id`                | Get agent                                |
| PUT    | `/agents/:id`                | Update agent                             |
| DELETE | `/agents/:id`                | Delete agent                             |
| POST   | `/agents/:id/chatgpt/login`  | Start ChatGPT OAuth login for this agent |
| POST   | `/agents/:id/chatgpt/logout` | Clear this agent's ChatGPT token         |
| GET    | `/agents/:id/chatgpt/status` | Check this agent's ChatGPT login status  |

Agent config fields:

- **Core**: `name`, `instructions`, `model`, `provider_type`, `auth_mode`, `api_key`, `base_url`
- **Model settings**: `model_settings` (JSON), `tool_use_behavior`, `disable_tool_choice_reset`, `max_turns`
- **Resilience**: `retry_enabled`, `retry_policy`, `fallback_models`
- **Error recovery**: `error_handlers` (JSON keyed by `max_turns` / `model_refusal` / `invalid_final_output`; each entry is `{"final_output": <JSON value>, "exclude_from_history": bool}` — the run completes with the static fallback instead of failing; `final_output` must be a string for plain-text agents or match `output_schema`)
- **Structured output**: `output_schema` (JSON Schema)
- **Guardrails**: `input_guardrails`, `output_guardrails`
- **Tools / Handoffs**: `tools` (JSON), `handoffs` (JSON), `handoff_description`, `handoff_input_filter`
- **Prompt**: `prompt_id`, `prompt_version` (OpenAI stored prompt)
- **Other**: `use_previous_response_id`, `max_tool_concurrency`, `tool_not_found_behavior`

Secret fields are masked on read — see [Secret handling](#secret-handling): the
`api_key` and each `fallback_models[].api_key`.

The ChatGPT OAuth routes are a per-agent capability (previously under `/chatgpt/*`
with an `?agent_config_id=` query parameter). The browser OAuth redirect lands at
`GET /api/v1/chatgpt/oauth/callback`.

### MCP Servers — `/api/v1/mcp-servers`

| Method | Path                           | Description                                                    |
|--------|--------------------------------|----------------------------------------------------------------|
| GET    | `/mcp-servers`                 | List servers with connection status                            |
| POST   | `/mcp-servers`                 | Create MCP config                                              |
| GET    | `/mcp-servers/:id`             | Get config                                                     |
| PUT    | `/mcp-servers/:id`             | Update config (toggles connect/disconnect on `enabled` change) |
| DELETE | `/mcp-servers/:id`             | Delete and disconnect                                          |
| POST   | `/mcp-servers/:id/connect`     | Connect (may trigger OAuth)                                    |
| DELETE | `/mcp-servers/:id/oauth-token` | Disconnect and clear the saved OAuth token ("sign out")        |
| GET    | `/mcp-servers/:id/tools`       | List tools exposed by the server                               |
| GET    | `/mcp-servers/oauth/callback`  | OAuth redirect callback                                        |

Transports: `stdio` and `streamable_http`. The HTTP transport supports
`auth_mode` `header` or `oauth`. Each server has an `enabled` flag (default
`true`); enabled servers are connected automatically on startup. Disabling a
server disconnects it; re-enabling triggers a reconnect — the toggle is the
on/off switch, there is no separate transient disconnect.

OAuth tokens obtained during authorization are persisted, so reconnecting (or
restarting the server) does not require re-authorizing. The list endpoint
reports `auth_state` per OAuth server: `authorized` (connected or a saved token
exists) or `unauthorized` (next connect opens the authorization popup). Use the
`oauth-token` DELETE endpoint — the "Clear auth" button in the server's edit
form — to drop the saved token, e.g. to re-authorize with a different account.

For `streamable_http` servers, the secret-bearing config fields are masked on
read — see [Secret handling](#secret-handling): every `headers` value and
`oauth_client_secret`.

### Memories — `/api/v1/memories`

| Method | Path            | Description                                |
|--------|-----------------|--------------------------------------------|
| GET    | `/memories`     | List memories                              |
| POST   | `/memories`     | Create memory (`key` + `content` required) |
| GET    | `/memories/:id` | Get memory                                 |
| PUT    | `/memories/:id` | Update memory                              |
| DELETE | `/memories/:id` | Delete memory                              |

Memories can be scoped to a specific agent via `agent_config_id`.

### Settings — `/api/v1/settings`

| Method | Path             | Description              |
|--------|------------------|--------------------------|
| GET    | `/settings`      | List all key-value pairs |
| GET    | `/settings/:key` | Get value                |
| PUT    | `/settings/:key` | Set value                |
| DELETE | `/settings/:key` | Delete                   |

Known keys:

- `proxy_url` — HTTP proxy for model and MCP calls
- `system_prompt` — global system prompt prefix
- `openai_api_key` — fallback provider key for agents that have no `api_key` of
  their own (secret; masked on read — see [Secret handling](#secret-handling))
- `brave_api_key` — injects a `brave_search` tool into all agents (secret; masked
  on read — see [Secret handling](#secret-handling))
- `trace_retention_days` — prune trace events older than N days (checked at
  startup and once a day); empty or `0` disables pruning
- `approval_ttl_minutes` — how long a pending tool approval may sit unanswered
  before it expires (default `1440` = 24h; `0` disables expiry)

### Skills — `/api/v1/skills` (read-only)

Discover skills under `{workspace}/skills/` and read their `SKILL.md`. This
resource is read-only; repo management lives under `/skill-repos`.

| Method | Path            | Description                                                      |
|--------|-----------------|------------------------------------------------------------------|
| GET    | `/skills`       | Discover skills under `{workspace}/skills/`                      |
| GET    | `/skills/*path` | Get SKILL.md content (path may be nested, e.g. `repo/sub-skill`) |

### Skill repos — `/api/v1/skill-repos`

Clone and maintain whole git repositories of skills.

| Method | Path                      | Description                                                                                          |
|--------|---------------------------|------------------------------------------------------------------------------------------------------|
| POST   | `/skill-repos`            | Clone — body `{url}` (http(s) only); `git clone --depth=1`, returns `201` with the discovered skills |
| POST   | `/skill-repos/:name/sync` | `git fetch && git reset --hard origin/HEAD` to update (discards local changes)                       |
| DELETE | `/skill-repos/:name`      | Remove the repo directory                                                                            |

Only `http(s)` remotes are accepted (`file://`, `ssh`, and git's `ext::`
transport are rejected). `sync` replaces the former `PUT /skills/:name`.

### Provider Routes — `/api/v1/provider-routes`

Map model-name prefixes to different API keys and base URLs for multi-provider
routing.

| Method | Path                   | Description  |
|--------|------------------------|--------------|
| GET    | `/provider-routes`     | List routes  |
| POST   | `/provider-routes`     | Create route |
| GET    | `/provider-routes/:id` | Get route    |
| PUT    | `/provider-routes/:id` | Update route |
| DELETE | `/provider-routes/:id` | Delete route |

The `api_key` field is masked on read — see [Secret handling](#secret-handling).

### Guardrails — `/api/v1/guardrails`

| Method | Path              | Description                             |
|--------|-------------------|-----------------------------------------|
| GET    | `/guardrails`     | List all guardrails (built-in + custom) |
| POST   | `/guardrails`     | Create custom guardrail                 |
| GET    | `/guardrails/:id` | Get guardrail                           |
| PUT    | `/guardrails/:id` | Update guardrail                        |
| DELETE | `/guardrails/:id` | Delete guardrail                        |

Types: `input` (pre-model) and `output` (post-model). Modes: `regex` (pattern
match triggers tripwire) and `max_length` (character limit).

Built-in: `content_filter` (input/regex — jailbreak keywords),
`max_input_length` (input/max_length — 50k chars),
`max_output_length` (output/max_length — 50k chars).

Guardrails attach at the **run level** (the agent's `input_guardrails` /
`output_guardrails` fields). Per-tool guardrails exist in the SDK but are not
yet configurable here — see [Roadmap](#roadmap).

### Sandboxes — `/api/v1/sandboxes`

| Method | Path                  | Description              |
|--------|-----------------------|--------------------------|
| GET    | `/sandboxes`          | List sandboxes           |
| POST   | `/sandboxes`          | Create sandbox           |
| GET    | `/sandboxes/:id`      | Get sandbox              |
| PUT    | `/sandboxes/:id`      | Update sandbox           |
| DELETE | `/sandboxes/:id`      | Delete sandbox           |
| POST   | `/sandboxes/:id/test` | Run health-check command |

Sandbox types: `local` (subprocess — requires `--allow-local-sandbox`), `docker`
(container), `ssh` (remote host). The `local` and `docker` host restrictions are
enforced on both create and update. For `ssh` sandboxes the `password` config
field is masked on read — see [Secret handling](#secret-handling).

### Playground — `/api/v1/playground`

| Method | Path                   | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
|--------|------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `/playground/generate` | One-off model call — `{agent_config_id, model?, system_instructions?, input_items, model_settings?, tools?}`; uses the agent's provider credentials, touches no session, records no run. `model_settings` overrides the agent's settings; `tools` are schema-only definitions (`{name, description?, parameters?}`) echoed from the traced request so the model can emit function calls — they are never executed. Backs the trace panel's "Replay" dialog. |

### ChatGPT OAuth

Login, logout, and status are per-agent, under the agent resource — see
[Agents](#agents--apiv1agents). The browser OAuth redirect callback is the only
top-level route:

| Method | Path                      | Description                           |
|--------|---------------------------|---------------------------------------|
| GET    | `/chatgpt/oauth/callback` | OAuth redirect callback (auth-exempt) |

### Secret handling

Secret fields are **write-only**. GET responses return them masked as `********`;
the plaintext is never sent to a client. On write:

- sending the `********` mask back keeps the currently stored value,
- sending a new value replaces it,
- sending `""` clears it.

This lets the UI round-trip whole objects without ever seeing the plaintext.
Masked fields: agent `api_key` and each `fallback_models[].api_key`,
provider-route `api_key`, MCP `headers` values and `oauth_client_secret`
(`streamable_http` only), SSH sandbox `password`, and the `brave_api_key` setting.

### Health

| Method | Path      | Description                                                    |
|--------|-----------|----------------------------------------------------------------|
| GET    | `/health` | Liveness probe (unauthenticated) — returns `{status, version}` |

### OpenAPI

A generated OpenAPI 3.1 document (YAML) is served at `GET /api/v1/openapi.yaml`
(unauthenticated). It is generated from swag annotations on the handlers via
`make openapi` in `cmd/agents-server`. There is intentionally no bundled
Swagger/Redoc UI — import the YAML into your own tool.

## WebSocket protocol

Endpoint: `GET /ws`

The WebSocket does not accept a token in the query string. After connecting, the
client must authenticate at the application level by sending
`{"type":"auth","token":"..."}` as the first message. The server replies with
`{"type":"auth.ok"}`.

All messages use the envelope format `{"type":"...", "payload":{...}}`.

Runs live in the runner's hub, independent of the connection. A dropped socket
does not cancel a run; after reconnecting, use `run.subscribe` to reattach to a
run's event stream (replaying buffered events after `from_seq`).

### Client → Server

| type            | Description                                                                                                     |
|-----------------|-----------------------------------------------------------------------------------------------------------------|
| `run.create`    | Start a run — `{session_id, input, agent_config_id?, sandbox_id?}`                                              |
| `run.subscribe` | (Re)attach to a run's event stream — `{run_id, from_seq?}` (omit `from_seq` or `0` replays everything retained) |
| `run.cancel`    | Cancel an in-flight run — `{run_id}`                                                                            |
| `tool.approve`  | Approve a pending tool call — `{tool_call_id}`                                                                  |
| `tool.reject`   | Reject a tool call — `{tool_call_id, reason?}`                                                                  |

### Server → Client

| type                    | Description                                                                                                                                             |
|-------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `run.started`           | Run begun — `{run_id, session_id}`                                                                                                                      |
| `run.agent_start`       | Agent taking its turn — `{run_id, agent_name}`                                                                                                          |
| `run.step`              | Streaming text delta — `{run_id, delta}`                                                                                                                |
| `run.reasoning`         | Streaming reasoning delta — `{run_id, delta}`                                                                                                           |
| `run.message`           | One completed assistant message: a turn's full text, interim narration or final answer, authoritative over its `run.step` deltas — `{run_id, text}`     |
| `run.reasoning_item`    | One completed reasoning block: a turn's full thinking text, authoritative over its `run.reasoning` deltas — `{run_id, text}`                            |
| `run.tool_call`         | Tool invoked — `{run_id, tool_call_id, tool_name, arguments, needs_approval}`                                                                           |
| `run.tool_result`       | Tool output — `{run_id, tool_call_id, output}`                                                                                                          |
| `run.handoff`           | Agent handoff — `{run_id, from, to}`                                                                                                                    |
| `run.compaction`        | Session compaction running at end of turn — `{run_id, phase: started\|finished, detail?}`                                                               |
| `run.output`            | Final output — `{run_id, final_output}`                                                                                                                 |
| `run.interrupted`       | Paused for tool approval (ends the stream for now; the decision resumes the SAME run id, continuing its event sequence) — `{run_id}`                                          |
| `run.error`             | Error — `{run_id?, session_id?, code, message}`; `session_id` is set when the failure precedes `run.started` (e.g. `session_busy`, `session_not_found`) |
| `run.cancelled`         | Cancelled — `{run_id}`                                                                                                                                  |
| `session.title_updated` | Title changed — `{session_id, title}`                                                                                                                   |
| `trace.span`            | Trace span — `{run_id, trace_id, span_id, error?, ...}`                                                                                                 |

Generation spans carry the full model request/response in their `data`
(`model`, `system_instructions`, `input`, `tools`, `model_settings`,
`handoffs`, `output_schema`, `output`) — the trace panel renders these when
you expand a generation span, so you can see exactly what each call sent
after compaction/filters, including MCP/skill tool definitions. Payloads past
512KB are replaced with a truncation marker; set
`OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA=false` to keep conversation
content out of traces entirely.

## Architecture

```
cmd/agents-server/
├── main.go                     entry point
├── cmd/root.go                 CLI flags & server bootstrap
├── internal/
│   ├── server/                 Gin engine, routing, auth middleware, WS upgrade
│   ├── handler/                HTTP handlers (one file per resource)
│   ├── bridge/                 business logic
│   │   ├── agent.go            assemble a full agent from DB config
│   │   ├── runner.go           stream execution, resume after approval
│   │   ├── run_hub.go          per-run event hub (buffering, seq resume, status)
│   │   ├── approvals.go        HITL approval persistence & resolution
│   │   ├── mcp_manager.go      MCP server connection lifecycle
│   │   ├── sandbox_manager.go  sandbox instance cache
│   │   ├── retention.go        approval-expiry reaper & trace pruning
│   │   └── ...                 tracing, guardrails, proxy, MCP/ChatGPT OAuth
│   ├── docs/                   generated OpenAPI 3.1 document, swagger.yaml (make openapi)
│   ├── store/                  SQLite data layer (bun ORM, 11 tables)
│   ├── protocol/               WebSocket message types
│   └── web/                    embedded SPA static files
└── skills/                     agent skills managed via API
```

### Request flow

1. A client starts a run — `run.create` over WebSocket or `POST /sessions/:id/runs`
   over REST. Both call `runner.StartRun`, which registers the run in the shared
   run hub and executes it in the background, independent of the caller's
   connection.
2. The runner loads config from the database and calls `BuildFullAgent` to
   assemble the agent with its provider, MCP tools, sandbox, guardrails,
   memories, and hooks, then calls the SDK's `agents.RunStreamed()` to execute.
3. Streaming events are published to the hub, which fans them out to every
   subscriber (WebSocket connections and SSE streams) and buffers them for replay
   so a reconnecting client can resume from a sequence number.
4. If a tool requires approval, the run pauses and the pending approval is
   persisted; it resumes on `approve`/`reject` (over either transport) and
   survives a server restart.
5. On completion the message history is persisted and the session title is
   auto-generated.

## Database

SQLite in WAL mode. Tables are created automatically on startup:

| Table               | Description                                                                         |
|---------------------|-------------------------------------------------------------------------------------|
| `sessions`          | Chat sessions                                                                       |
| `messages`          | Conversation message history                                                        |
| `agent_configs`     | Agent configurations                                                                |
| `mcp_servers`       | MCP server configurations                                                           |
| `memories`          | Agent memories                                                                      |
| `settings`          | Global key-value settings                                                           |
| `provider_routes`   | Model-prefix routing rules                                                          |
| `sandbox_configs`   | Sandbox configurations                                                              |
| `guardrails`        | Custom guardrail definitions                                                        |
| `trace_events`      | Trace spans (agent, generation, function, handoff, compaction)                      |
| `pending_approvals` | Runs paused for human-in-the-loop tool approval (persisted so they survive restart) |

The database file can be deleted and recreated freely — there is no migration
mechanism.

## Roadmap

- **Tool-level guardrail config.** The SDK supports per-tool guardrails
  (`FunctionTool.InputGuardrails` / `OutputGuardrails`), but the server only
  wires run-level ones today. Plan: let the agent config attach guardrails to
  individual tools (by tool name), with matching UI in the agent panel. Once
  that lands, also expose `RunOptions.PreApprovalToolInputGuardrails` as an
  agent config field, so a guardrail rejection can resolve an approval-gated
  call without a human round-trip.
- **Render tool-output custom data.** The SDK's
  `FunctionTool.CustomDataExtractor` attaches SDK-only metadata (renderer
  hints, record IDs) to `ToolCallOutputItem.CustomData` without sending it to
  the model. The chat UI's tool-call cards could consume it for rich rendering
  (tables, charts) once any built-in or server-defined tool starts producing it.
