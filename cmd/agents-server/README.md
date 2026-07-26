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
- [Design invariants](#design-invariants) — the rules every panel/handler pair must follow
- [Database](#database)
- [Roadmap](#roadmap)

## Quick start

Grab a prebuilt binary for your platform from the
[Releases](https://github.com/zzir/agents-go/releases) page, or build from source:

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
| DELETE | `/sessions/:id`           | Delete session and its entries and traces                            |
| GET    | `/sessions/:id/messages`  | List session entries (paginated)                                     |
| GET    | `/sessions/:id/traces`    | List trace events (paginated)                                        |
| POST   | `/sessions/:id/fork`      | Fork session                                                         |
| POST   | `/sessions/:id/runs`      | Start a run on the session (see [Runs](#runs--apiv1runs))            |
| GET    | `/sessions/:id/approvals` | List pending approvals (see [Approvals](#approvals--apiv1approvals)) |
| GET    | `/sessions/:id/tasks`     | List background tasks spawned from the session (see below)          |

`POST /sessions` accepts an optional `agent_config_id` to bind the session to an
agent at creation (it must reference an existing agent). Rename and pin are a
single `PATCH /sessions/:id` accepting a partial `{name?, pinned?}` body; both
the separate `PUT` rename and `PATCH /sessions/:id/pin` endpoints are gone.

`fork` copies the source session's entries (and their traces) into a new
session. Its body is optional: `{message_id?, exclusive?, label?}`. Omit
`message_id` to fork everything; supply it to bound the copy up to and including
that entry (`exclusive: true` excludes the boundary entry itself). Entry ids and
their parent links are rewritten into the fork's namespace, so the copy is a
self-consistent tree rather than one pointing back at another session. The
session inherits the source's `agent_config_id`.

`/sessions/:id/messages` returns **session entries** — the SDK's
`agents.SessionEntry` as the runner wrote it, plus the row id the cursor pages
on. Each carries its `kind` (`item` / `annotation` / `compaction` / …), its
recorded `display`, and its `usage` / `diagnostics`. Update entries are folded
into their targets server-side, so a client never applies them itself. The path
keeps its name for compatibility.

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
- **Model settings**: `model_settings` (JSON), `stop_at_tools`, `disable_tool_choice_reset`, `max_turns`
- **Resilience**: `retry_enabled`, `retry_policy`, `fallback_models`
- **Error recovery**: `error_handlers` (JSON keyed by `max_turns` / `model_refusal` / `invalid_final_output`; each entry is `{"final_output": <JSON value>, "exclude_from_history": bool}` — the run completes with the static fallback instead of failing; `final_output` must be a string for plain-text agents or match `output_schema`)
- **Structured output**: `output_schema` (JSON Schema)
- **Guardrails**: `guardrails` (JSON array of names — one list, since a guardrail carries the stages it inspects)
- **Tools / Handoffs**: `tools` (JSON), `handoffs` (JSON), `handoff_description`, `handoff_input_filter`
- **Prompt**: `prompt_id`, `prompt_version` (OpenAI stored prompt)
- **Other**: `use_previous_response_id`, `max_tool_concurrency`, `tool_not_found_behavior`

Secret fields are masked on read — see [Secret handling](#secret-handling): the
`api_key` and each `fallback_models[].api_key`.

The ChatGPT OAuth routes are a per-agent capability (previously under `/chatgpt/*`
with an `?agent_config_id=` query parameter). The browser OAuth redirect lands at
`GET /api/v1/chatgpt/oauth/callback`.

### MCP Servers — `/api/v1/mcp-servers`

| Method | Path                           | Description                                                       |
|--------|--------------------------------|-------------------------------------------------------------------|
| GET    | `/mcp-servers`                 | List servers with derived `status`                                |
| POST   | `/mcp-servers`                 | Create MCP config (an enabled server connects in the background)  |
| GET    | `/mcp-servers/:id`             | Get config                                                        |
| PUT    | `/mcp-servers/:id`             | Update config (reconciles the live connection to the new config)  |
| DELETE | `/mcp-servers/:id`             | Delete and disconnect                                             |
| POST   | `/mcp-servers/:id/connect`     | Connect (may trigger OAuth); `409` if the server is disabled      |
| DELETE | `/mcp-servers/:id/oauth-token` | Disconnect and clear the saved OAuth token ("sign out")           |
| GET    | `/mcp-servers/:id/tools`       | List tools exposed by the server                                  |
| GET    | `/mcp-servers/oauth/callback`  | OAuth redirect callback                                           |

Transports: `stdio` and `streamable_http`. The HTTP transport supports
`auth_mode` `header` or `oauth`. Enabled servers are connected automatically on
startup and after create/update; disabling disconnects. A disabled server
cannot be connected (`409`) — agents pick tools by live connection, so the
toggle is a hard off switch.

Every read endpoint reports a single derived `status` per server: `disabled`,
`connecting` (handshake in flight), `authorizing` (OAuth popup pending user
action), `needs_auth` (OAuth without a saved token — connect returns an
authorize URL), `disconnected` (enabled but no live connection), or
`connected`. Writes reconnect in the background, so the status returned by a
PUT/POST is often still `disconnected` or `connecting` — poll the list until it
settles (the built-in UI does exactly that). While `authorizing`, calling
connect again is safe and intended: it supersedes the stale attempt (e.g. the
user closed the popup, which sends no signal) and returns a fresh authorize
URL; an abandoned attempt otherwise expires on its own after 5 minutes.

OAuth tokens obtained during authorization are persisted and reported as
`has_oauth_token`, so reconnecting — including the automatic reconnect after a
disable/enable cycle or a restart — needs no re-authorization. Use the
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

A guardrail carries `stages` — where it runs — and one definition can cover
several: `input` (the run input, pre-model), `output` (the final output),
`tool_input` (a tool call's arguments, before the tool runs) and `tool_output`
(a tool's result, before the model reads it). A content scanner that should see
the input, the tool arguments and the final output is ONE guardrail with three
stages, which is the SDK's model — naming it three times would be three
near-identical definitions to keep in sync.

Modes: `regex` (pattern match triggers tripwire) and `max_length` (character
limit). Both inspect whatever the stage puts under them.

`blocking` applies at the input stage only: it runs the guardrail to completion
before the first model call (a gate) instead of racing it, so a tripwire
prevents the call and any token spend.

A guardrail's **name is its identity** — an agent config references it by name
and nothing else, so names are unique across all definitions.

Built-in: `content_filter` (input + tool_input, regex — jailbreak keywords),
`max_input_length` (input, 50k chars), `max_output_length` (output, 50k chars).

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

Every response carries a computed `terminal` boolean — whether the sandbox can
host an interactive web terminal (`ssh` always, `docker` only with
`persistent: true`, `local` never, by design). The chat composer shows the
terminal toggle only when the selected sandbox advertises it; the session
itself runs over [`/ws/terminal`](#terminal-endpoint--wsterminal).

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

> The target shape this protocol is moving toward — one `run.entry` event,
> entry ids on streaming deltas, SDK-owned error codes — is frozen in
> [PROTOCOL.md](PROTOCOL.md). What follows is what ships today.

The WebSocket does not accept a token in the query string. After connecting, the
client must authenticate at the application level by sending
`{"type":"auth","token":"..."}` as the first message. The server replies with
`{"type":"auth.ok"}`.

All messages use the envelope format `{"type":"...", "payload":{...}}`.

Runs live in the runner's hub, independent of the connection, and their events
are a **broadcast bus**: every authenticated connection is attached to every
run's stream — on connect (all in-flight runs, with a replay of their buffered
events) and automatically when any run starts or resumes, no matter which
connection (or REST call) started it. Two browsers on the same session both
watch the conversation live. A dropped socket does not cancel a run; after
reconnecting the server re-attaches the connection, and `run.subscribe` remains
available to resume from a specific cursor (`from_seq`) without a full replay.

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
| `run.started`           | Run begun — `{run_id, session_id, input}`; `input` is the user prompt, so a browser that didn't send it can render the user bubble. A background task run additionally carries `{parent_session_id, parent_run_id, tool_call_id, label}` — the client routes it into the parent session's task list, never a chat timeline |
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

### Terminal endpoint — `GET /ws/terminal`

One interactive sandbox terminal per connection, deliberately separate from
the `/ws` event bus (different delivery semantics: an ordered byte stream with
backpressure, not broadcast envelopes with replay). Authentication is the same
first-message `auth` handshake.

After `auth.ok` the client sends exactly one control envelope and the server
answers:

| type              | Direction | Description                                                                  |
|-------------------|-----------|------------------------------------------------------------------------------|
| `terminal.open`   | C → S     | Start the session — `{sandbox_id, cols?, rows?}`; must be the first message  |
| `terminal.ready`  | S → C     | Shell is live; binary frames flow from here                                  |
| `terminal.error`  | S → C     | Open failed (unknown sandbox, `local` type, non-persistent docker, backend error) |
| `terminal.resize` | C → S     | PTY resize — `{cols, rows}`                                                  |
| `terminal.exit`   | S → C     | Shell exited — `{code}` (`-1` when unknown); the server then closes          |

**Binary WebSocket frames carry the terminal byte stream in both directions**
(client → stdin, PTY output → client); text frames are reserved for the JSON
control envelopes above. `local` sandboxes are refused server-side regardless
of `--allow-local-sandbox` — a browser shell on the host process is a bigger
grant than that flag implies. Terminals are capped at 4 per sandbox config,
and updating or deleting a sandbox closes its live terminals.

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

## Design invariants

Cross-cutting rules every resource (panel + handler + store + bridge consumer)
must follow. Each exists because its violation shipped a real bug; the fix for
a new feature is to fit these shapes, not to add a one-off patch beside them.
When a change genuinely doesn't fit, update this list in the same PR.

**API shape**

1. **`config` blobs travel as JSON objects, never strings.** Every
   backend-specific settings blob (`mcp_servers.config`, `sandbox_configs.config`,
   `guardrails.config`) is a `json.RawMessage` exchanged as an inline JSON
   object. The frontend reads and writes it as an object — no
   `JSON.stringify`/`JSON.parse` of the field itself. (The guardrail panel once
   sent a stringified config; every save failed with 400 and nobody noticed.)
2. **List responses carry every field the edit form needs.** `useCrud` panels
   initialize their edit form from the list item. A list-side projection that
   drops fields (config, flags) makes the edit form silently wipe them on the
   next save. Either return full rows from List or make the panel fetch Get
   before editing — never assume "the list is just for display".
3. **Derived state is computed in one backend function; the frontend renders it
   verbatim.** Connection/login/authorization lifecycle is reported as a single
   `status` (MCP: `disabled | connecting | authorizing | needs_auth |
   disconnected | connected`) or boolean (`chatgpt_logged_in`,
   `has_oauth_token`) derived server-side from the facts it already owns
   (manager/coordinator state + stored config). The frontend must not
   reconstruct state by combining multiple response fields or its own
   per-item maps — that is how phantom states (`auth_state === 'authorizing'`)
   and stuck buttons happen.
4. **Keep swagger annotations in sync with the actual response type**, and run
   `make openapi` after any handler change — CI diffs the generated spec.

**State & lifecycle**

5. **An off switch must hold at every entrance.** When a resource has
   `enabled=false`, every path that could activate it (manual connect
   endpoints, agent assembly, startup auto-connect) must respect the flag —
   agents pick MCP tools by live connection, so one unguarded connect path
   voids the whole switch.
6. **Create and Update trigger the same side effects.** If updating a resource
   reconciles a live connection, creating one must too. Asymmetry here reads
   as "sometimes it just doesn't work".
7. **Async settling uses grace-window polling, not per-item timers.** After a
   mutation whose effect completes in the background (reconnect, OAuth), the
   panel polls the list while any row is in a transitional status or an
   ~8s post-mutation grace window is open, then stops. One-shot notifications
   (popup `postMessage`) only trigger an immediate reload — they must not own
   state cleanup, because they don't always arrive. Long-lived per-item
   `setInterval` + hand-rolled cleanup is the pattern that produced the
   5-minute stuck "Authorizing..." button.
8. **In-progress buttons stay retryable when the wait is on an external actor.**
   If completion depends on the user finishing a popup flow, the button that
   started it must allow a superseding retry (cancel the stale attempt, start
   fresh) instead of disabling itself until a timeout.

**Secrets**

9. **Secrets are write-only and go through `handler/secrets.go`.** Read side
   masks with `********`; write side resolves the sentinel (mask = keep stored,
   `""` = clear, anything else = replace). New secret fields must use the same
   sanitize/restore helpers and get a round-trip test — no ad-hoc masking.
10. **OAuth-class tokens never leave the server.** Store them in their own
    column with `json:"-"`, exclude the column from CRUD updates
    (`ExcludeColumn`), and expose only a derived boolean
    (`has_oauth_token`, `chatgpt_logged_in`). Do not reuse a masked token
    string as a truthiness signal.

**Store layer**

11. **No bun `default:` tags on booleans.** bun swaps a zero-value field for
    SQL `DEFAULT` on insert, so `default:true` silently enables a row created
    with `enabled=false`. Use `notnull` and set the value in Go.
12. **Deleting a referenced resource fails loud at use, never silently skips a
    safety feature.** Guardrail names that no longer resolve fail the agent
    build (a guardrail that appears enabled but never runs is a security hole);
    dangling MCP/skill ids are filtered with a visible count in the UI. Pick
    one of those two behaviors deliberately for any new reference.

**Chat / run streaming**

13. **Run events are a broadcast bus, not a reply channel.** Every
    authenticated WS connection is attached to every run's stream — on
    connect (all live runs, with replay) and through `Runner.OnRunAttach`
    when a run starts or resumes, whether it was created over WS or REST.
    Two browsers on the same session both watch the conversation live;
    `run.started` carries the prompt (`input`) so a browser that didn't send
    it can render the user bubble. Never wire an event to "the connection
    that asked" — that is exactly the bug this replaced.
14. **Protocol constants have one definition per side.** Event types
    (`run.error`, …) and error codes (`session_busy`, …) live in
    `internal/protocol` (Go) and `src/lib/protocol.ts` (TS mirror). Emitters
    and consumers reference the constants, never string literals — a typo must
    be a compile error, not an event that silently never fires. Adding an
    event means updating both files.
15. **A streamed turn must equal its reload.** The streaming path
    (`src/lib/streamReducer.ts` pure transforms, applied by `useAgentSocket`)
    and the replay path (`buildTimeline` over persisted ENTRIES) must produce
    the same `turn.parts`; `src/lib/timeline.test.ts` pins this isomorphism — run
    it via `npm test`. Intentional differences are documented and asserted
    there (currently: handoff parts are live-only; a rejected call's status
    replays as completed). A new part type or field lands on BOTH paths plus
    the shared types in `timeline.ts`, or the test fails.
16. **Terminal run events reconcile against the store.** Every terminal event
    handler (output/error/cancelled) applies its optimistic parts and then
    reloads the persisted timeline as the authority. Exceptions must be
    deliberate and listed here — currently only `guardrail_tripwire`, which
    skips the reload to keep the retracted-answer view the SDK never persists.
17. **The streaming block patches the DOM; user intent beats the pin.** The
    live text is morphdom-patched (`StreamingMarkdown.tsx`), never rewritten
    via innerHTML — node identity is what keeps a text selection alive across
    deltas, so anything that replaces those nodes wholesale is a regression.
    Bottom-following (`useScrollToBottom`) re-fires on every content growth
    (the dep includes streamed text length) and yields to explicit user
    intent: an upward wheel/drag or an actively changing selection unsticks
    immediately; a stale leftover selection must NOT block re-sticking when
    the user scrolls back down (recency windows, not standing state, arbitrate
    the races with the pin's own trailing scroll events).

**Background tasks**

18. **A task is a durable entity; a run is one execution of it.** `spawn_task`
    mints separate ids: the task row carries `run_id` (its current attempt),
    and `run.started` / `RunInfo.task` carry `task_id` — clients route events
    by run id and key task state by task id (a future retry mints a new run id
    on the same task row). The transcript lives in a hidden child session. The
    spawn target is an agent config by name; an empty `agent_name` (or the
    `default` / `self` / `current` aliases) runs the task with the spawning
    run's own agent — a config actually named that way wins. Task events use
    the same broadcast bus, replay cursors, approval persistence, and
    retention as chat runs — a task-specific transport is how the two
    lifecycles would drift.
19. **The spawn card's durable truth is an appended update entry.** The hub's
    RunInfo is GC'd minutes after a run ends; when a task changes state the
    server APPENDS an update entry carrying
    `{task_id, task_label, task_status, task_summary}` addressed to the spawn
    call's id, and the read folds it into that call's display. Appending is
    what removed the retry loop: a fast task can finish before the turn that
    spawned it is persisted, and the old rewrite hunted for a row that did not
    exist yet. An update may be stored BEFORE its target; folding associates
    them by call id afterwards. A non-terminal update is dropped when the task
    row is already terminal, so a reordered notify cannot roll a finished card
    back. Live status comes from run events; durable status comes from the
    folded entry — never from the hub after the fact. Completion wakes
    the parent at its next run boundary via a `[task-notification] ` input;
    the debt is the row's `notify_state` (pending → consumed by an in-turn
    `task_status` read, or → delivered by the wake-up run), written in the
    same UPDATE as the terminal status — the auto-wake survives restarts via
    the startup sweep. The notification is ordinary user-role input identified by its text
    prefix. It never renders in the timeline — the composer's task indicator
    and the Inspector are the human-facing surfaces; the model reads the text
    verbatim. The prefix carries no privileged behavior: a user typing it
    merely hides their own message from the transcript view.
20. **The right side panel is a single-instance Inspector.** Traces, the task
    list, and one task's detail (live transcript + trace, assembled with the
    same streamReducer/timeline code as the chat) are lenses of one panel —
    a new inspection surface is a new lens, not a second drawer. Task detail
    accumulates live child-run events only while open (watchTask/unwatchTask).
21. **A task's terminal state is written exactly once, via row CAS.** The
    durable row is the terminal authority: `Finalize` (status + full result +
    notification debt in one UPDATE) only wins while the row is non-terminal,
    stop/approve claims race through the same CAS (`Finalize` vs
    `ReclaimWorking` — exactly one wins, and `hub.resume` refuses finished
    records as the second line), and `task_status` treats only the row as
    final — a hub-terminal run whose row hasn't landed is still `working`. A
    graceful stop marks the hub record before signalling, so its clean finish
    lands as `cancelled` ("stopped after the current turn"), never as a
    completion. Cancellations consume their own wake-up debt (the user did it;
    completed / failed are the states worth waking the parent for). Deleting a
    session stops its run tree first (cancel + bounded wait on the done gate)
    so no write can land after the cascade.
22. **One entry in, the same entry out.** The `entries` table stores whole
    `agents.SessionEntry` JSON, with only the columns the queries need lifted
    out. The server does not re-derive a display, a role, or provenance at read
    time — the runner already decided all three, and a reader that recomputes
    them can only produce a worse version that drifts. The messages table this
    replaced had a column per field the UI wanted, so `Source`, `Usage`,
    `Diagnostics`, `NestedUsage` and the parent link had nowhere to go and were
    dropped on the way in. Compaction soft-deletes (`compacted = true`) so the
    UI can still show what was folded, and appends a compaction CHECKPOINT
    whose payload carries the retained tail — which is why the model sees
    `[summary, kept…]` by construction rather than because the reader hoists a
    row to the front. The checkpoint also names what it folded
    (`compaction.excluded_ids`), and the timeline moves those entries INSIDE
    it: a reader scrolling back sees one "~12k → ~3k tokens" marker, not the
    folded turns rendered as though the model still reads them. They stay real
    and one expand away — an entry marked compacted that no checkpoint names
    renders in place, because history is not what compaction deletes.
23. **Schema changes ship without migrations.** `CREATE TABLE / INDEX IF NOT
    EXISTS` is the whole story; a structural change to an existing table means
    dropping and recreating the database (dev-tool stance, decided
    deliberately). Never add ALTER TABLE migration machinery here.

## Database

SQLite in WAL mode. Tables are created automatically on startup:

| Table               | Description                                                                         |
|---------------------|-------------------------------------------------------------------------------------|
| `sessions`          | Chat sessions                                                                       |
| `entries`           | Session entries (the conversation, annotations and compaction checkpoints)          |
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

- **Guardrail ordering at the approval gate.** The tool stages are configurable
  now (a guardrail's `stages` cover `tool_input` / `tool_output` for every tool
  call), but `RunOptions.PreApprovalToolInputGuardrails` is not exposed as an
  agent config field. With it on, a guardrail rejection resolves an
  approval-gated call without a human round-trip. Per-TOOL binding — "only this
  tool's arguments go through this guardrail" — is a separate thing the SDK
  does not model; it would need a `Stages`-like selector keyed by tool name.
- **Render tool-output custom data.** The SDK's
  `FunctionTool.CustomDataExtractor` attaches SDK-only metadata (renderer
  hints, record IDs) to `ToolCallOutputItem.CustomData` without sending it to
  the model. The chat UI's tool-call cards could consume it for rich rendering
  (tables, charts) once any built-in or server-defined tool starts producing it.
