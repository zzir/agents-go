# agents-server

A demo application for the [agents-go](../../README.md) SDK. It provides a REST
API, WebSocket streaming, and an embedded SPA that lets you configure agents, MCP
servers, sandboxes, memories, and skills — then run conversations in a browser.

![screenshot](screenshot.png)

## Quick start

```bash
go build -o agents-server ./cmd/agents-server
./agents-server --port 8080
```

On startup the server prints an auto-generated auth token. Open
`http://127.0.0.1:8080` to access the web UI.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | `8080` | HTTP listen port |
| `--db` | `data.db` | SQLite database file |
| `--root-dir` | `.` | Root directory for skills and file browsing |
| `--token` | auto | Auth token; randomly generated when omitted |

## Authentication

All `/api/*` requests require a token, passed via:

- `Authorization: Bearer <token>` header
- `?token=<token>` query parameter

## REST API

Base path `/api/`. All request and response bodies are JSON.

### Sessions — `/api/sessions`

| Method | Path | Description |
|---|---|---|
| GET | `/sessions` | List sessions |
| POST | `/sessions` | Create session |
| GET | `/sessions/:id` | Get session |
| PUT | `/sessions/:id` | Rename session |
| DELETE | `/sessions/:id` | Delete session and its traces |
| GET | `/sessions/:id/messages` | List conversation messages |
| GET | `/sessions/:id/traces` | List trace events |

### Agents — `/api/agents`

| Method | Path | Description |
|---|---|---|
| GET | `/agents` | List agent configs |
| POST | `/agents` | Create agent |
| GET | `/agents/:id` | Get agent |
| PUT | `/agents/:id` | Update agent |
| DELETE | `/agents/:id` | Delete agent |

Agent config fields:

- **Core**: `name`, `instructions`, `model`, `provider_type`, `auth_mode`, `api_key`, `base_url`
- **Model settings**: `model_settings` (JSON), `tool_use_behavior`, `disable_tool_choice_reset`, `max_turns`
- **Resilience**: `retry_enabled`, `retry_policy`, `fallback_models`
- **Structured output**: `output_schema` (JSON Schema)
- **Guardrails**: `input_guardrails`, `output_guardrails`
- **Tools / Handoffs**: `tools` (JSON), `handoffs` (JSON), `handoff_description`, `handoff_input_filter`
- **Prompt**: `prompt_id`, `prompt_version` (OpenAI stored prompt)
- **Other**: `use_previous_response_id`, `max_tool_concurrency`, `tool_not_found_behavior`

### MCP Servers — `/api/mcp-servers`

| Method | Path | Description |
|---|---|---|
| GET | `/mcp-servers` | List servers with connection status |
| POST | `/mcp-servers` | Create MCP config |
| GET | `/mcp-servers/:id` | Get config |
| PUT | `/mcp-servers/:id` | Update config |
| DELETE | `/mcp-servers/:id` | Delete and disconnect |
| POST | `/mcp-servers/:id/connect` | Connect (may trigger OAuth) |
| POST | `/mcp-servers/:id/disconnect` | Disconnect |
| GET | `/mcp-servers/:id/tools` | List tools exposed by the server |

Transports: `stdio` and `streamable_http`. The HTTP transport supports
`auth_mode` `header` or `oauth`.

### Memories — `/api/memories`

| Method | Path | Description |
|---|---|---|
| GET | `/memories` | List memories |
| POST | `/memories` | Create memory (`key` + `content` required) |
| GET | `/memories/:id` | Get memory |
| PUT | `/memories/:id` | Update memory |
| DELETE | `/memories/:id` | Delete memory |

Memories can be scoped to a specific agent via `agent_config_id`.

### Settings — `/api/settings`

| Method | Path | Description |
|---|---|---|
| GET | `/settings` | List all key-value pairs |
| GET | `/settings/:key` | Get value |
| PUT | `/settings/:key` | Set value |
| DELETE | `/settings/:key` | Delete |

Known keys: `openai_api_key` (global API key fallback), `system_prompt`
(global system prompt prefix), `proxy_url` (HTTP proxy for model and MCP calls).

### Skills — `/api/skills`

| Method | Path | Description |
|---|---|---|
| GET | `/skills` | Discover skills under `{root-dir}/skills/` |
| GET | `/skills/*path` | Get SKILL.md content |
| POST | `/skills/clone` | `git clone --depth=1` a skill repo |
| PUT | `/skills/:name` | `git fetch && git reset --hard` to update |
| DELETE | `/skills/:name` | Remove skill directory |

### Files — `/api/files`

| Method | Path | Description |
|---|---|---|
| GET | `/files?path=...` | List directory entries |
| GET | `/files/*path` | Read file content (max 1 MB) |

Paths are confined to `--root-dir`; directory traversal is rejected.

### Provider Routes — `/api/provider-routes`

Map model-name prefixes to different API keys and base URLs for multi-provider
routing.

| Method | Path | Description |
|---|---|---|
| GET | `/provider-routes` | List routes |
| POST | `/provider-routes` | Create route |
| PUT | `/provider-routes/:id` | Update route |
| DELETE | `/provider-routes/:id` | Delete route |

### Guardrails — `/api/guardrails`

| Method | Path | Description |
|---|---|---|
| GET | `/guardrails` | List available guardrails |

Built-in: `content_filter` (keyword patterns), `max_input_length` (50k chars),
`max_output_length` (50k chars).

### Sandboxes — `/api/sandboxes`

| Method | Path | Description |
|---|---|---|
| GET | `/sandboxes` | List sandboxes |
| POST | `/sandboxes` | Create sandbox |
| GET | `/sandboxes/:id` | Get sandbox |
| PUT | `/sandboxes/:id` | Update sandbox |
| DELETE | `/sandboxes/:id` | Delete sandbox |
| POST | `/sandboxes/:id/exec` | Execute code |

Sandbox types: `local` (subprocess), `docker` (container), `ssh` (remote host).

## WebSocket protocol

Endpoint: `GET /ws?token=<token>`

After connecting, the client must send `{"type":"auth","token":"..."}` as the
first message. The server replies with `{"type":"auth.ok"}`.

All messages use the envelope format `{"type":"...", "payload":{...}}`.

### Client → Server

| type | Description |
|---|---|
| `run.create` | Start a run — `{session_id, input, agent_config_id?, sandbox_id?}` |
| `run.cancel` | Cancel an in-flight run — `{run_id}` |
| `tool.approve` | Approve a pending tool call — `{tool_call_id}` |
| `tool.reject` | Reject a tool call — `{tool_call_id, reason?}` |

### Server → Client

| type | Description |
|---|---|
| `run.started` | Run begun — `{run_id, session_id}` |
| `run.agent_start` | Agent taking its turn — `{run_id, agent_name}` |
| `run.step` | Streaming text delta — `{run_id, delta}` |
| `run.tool_call` | Tool invoked — `{run_id, tool_call_id, tool_name, arguments, needs_approval}` |
| `run.tool_result` | Tool output — `{run_id, tool_call_id, output}` |
| `run.handoff` | Agent handoff — `{run_id, from, to}` |
| `run.output` | Final output — `{run_id, final_output}` |
| `run.error` | Error — `{run_id, code, message}` |
| `run.cancelled` | Cancelled — `{run_id}` |
| `session.title_updated` | Title changed — `{session_id, title}` |
| `hook.event` | Lifecycle hook — `{run_id, hook, agent_name?, ...}` |
| `trace.span` | Trace span — `{run_id, trace_id, span_id, ...}` |

## Architecture

```
cmd/agents-server/
├── main.go                     entry point
├── cmd/root.go                 CLI flags & server bootstrap
├── internal/
│   ├── server/                 Gin engine, routing, auth middleware, WS upgrade
│   ├── handler/                HTTP handlers (one file per resource)
│   ├── bridge/                 business logic
│   │   ├── runner.go           build agent, stream execution, resume after approval
│   │   ├── mcp_manager.go      MCP server connection lifecycle
│   │   ├── sandbox_manager.go  sandbox instance cache
│   │   ├── oauth.go            MCP OAuth coordinator
│   │   └── ...                 hooks, tracer, guardrails, proxy
│   ├── store/                  SQLite data layer (bun ORM, 9 tables)
│   ├── protocol/               WebSocket message types
│   └── web/                    embedded SPA static files
└── skills/                     agent skills managed via API
```

### Request flow

1. Client sends `run.create` over WebSocket
2. `WSHandler` calls `runner.RunStreamed()`
3. Runner loads config from the database and calls `BuildFullAgent` to assemble
   the agent with its provider, MCP tools, sandbox, guardrails, memories, and hooks
4. Calls the SDK's `agents.RunStreamed()` to execute
5. Streaming events are pushed to the client as WebSocket envelopes
6. If a tool requires approval, the run pauses until `tool.approve` / `tool.reject`
7. On completion the message history is persisted and the session title is
   auto-generated

## Database

SQLite in WAL mode. Tables are created automatically on startup:

| Table | Description |
|---|---|
| `sessions` | Chat sessions |
| `messages` | Conversation message history |
| `agent_configs` | Agent configurations |
| `mcp_server_configs` | MCP server configurations |
| `memories` | Agent memories |
| `settings` | Global key-value settings |
| `provider_routes` | Model-prefix routing rules |
| `sandbox_configs` | Sandbox configurations |
| `trace_events` | Trace and hook events |

The database file can be deleted and recreated freely — there is no migration
mechanism.
