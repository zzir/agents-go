# agents-server — the agents-go workbench

**Go agents. Local first.** The Go-native agent workbench you run yourself,
built on the [agents-go](../../README.md) SDK: one binary, your data (SQLite
or PostgreSQL), an embedded Primer-styled UI. Run agents and workflows in a
sandbox, behind tool approvals; debug with traces, replay & fork; solo or as a
team. Configure agents, providers, MCP servers, sandboxes,
guardrails, memories, skills and workflows; run conversations with streaming
output, tool approval, traces, a context lens, replay, interactive sandbox
terminals and background tasks. This page is the manual — flags, REST API,
WebSocket protocol, architecture, and the design invariants every
panel/handler pair follows.

![screenshot](screenshot.png)

## Contents

- [Quick start](#quick-start) — build, flags, [deployment](#deployment)
- [Authentication](#authentication)
- [REST API](#rest-api) — [errors](#errors) · [conventions](#response-conventions) · [sessions](#sessions--apiv1sessions) · [runs / SSE](#runs--apiv1runs) · [approvals](#approvals--apiv1approvals) · [tasks](#tasks--apiv1tasks) · [agents](#agents--apiv1agents) · [MCP servers](#mcp-servers--apiv1mcp-servers) · [memories](#memories--apiv1memories) · [settings](#settings--apiv1settings) · [skills](#skills--apiv1skills-read-only) · [skill repos](#skill-repos--apiv1skill-repos) · [providers](#providers--apiv1providers) · [workflows](#workflows--apiv1workflows) · [guardrails](#guardrails--apiv1guardrails) · [sandboxes](#sandboxes--apiv1sandboxes) · [playground](#playground--apiv1playground) · [secret handling](#secret-handling) · [OpenAPI](#openapi)
- [WebSocket protocol](#websocket-protocol)
- [Architecture](#architecture)
- [Design invariants](#design-invariants) — the rules every panel/handler pair must follow
- [Database](#database)
- [Roadmap](#roadmap)

## Quick start

Grab a prebuilt binary for your platform from the
[Releases](https://github.com/zzir/agents-go/releases) page, or build from
source. The web UI is compiled into the binary via `go:embed`, and the built
`frontend/dist` is not checked in — so a source build must build the frontend
first; `make build` does both (npm required):

```bash
cd cmd/agents-server
make build          # npm install + build the SPA, then go build with it embedded
./agents-server --port 9527
```

On startup the server prints an auto-generated auth token. Open
`http://127.0.0.1:9527` to access the web UI.

### Flags

| Flag                    | Default     | Description                                            |
|-------------------------|-------------|--------------------------------------------------------|
| `--host`                | `127.0.0.1` | Bind address (use `0.0.0.0` for LAN access)            |
| `--port`                | `9527`      | HTTP listen port                                       |
| `--db`                  | `data.db`   | SQLite file path, or a `postgres://` DSN               |
| `--workspace`           | `.`         | Workspace directory for skills and file operations     |
| `--token`               | auto        | Auth token; randomly generated when omitted            |
| `--allow-local-sandbox` | `false`     | Allow creating local (non-isolated) sandboxes          |
| `--max-tasks`           | `0`         | Max live background tasks per session (`0` = default 6) |
| `--log-level`           | `info`      | `debug`, `info`, `warn` or `error`                     |
| `--log-format`          | `text`      | `text` for a terminal, `json` for a collector          |
| `--base-url`            | —           | Public origin, `scheme://host[:port]` — see [Deployment](#deployment) |
| `--trusted-proxies`     | —           | Comma-separated proxy IPs/CIDRs whose `X-Forwarded-For` is believed for client IPs |
| `--auth`                | `token`     | `token` (single static token) or `oauth` (per-user login — see [OAuth mode](#oauth-mode)) |
| `--oauth-google-client-id` / `--oauth-google-client-secret` | — | Google login credentials (secret also via `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET`) |
| `--allowed-domains` / `--allowed-emails` | — | OAuth admission allowlist (comma-separated)  |
| `--bootstrap-admin`     | —           | Email that signs in as admin; implicitly admitted        |
| `--audit-retention-days` | `0`        | Prune audit log entries older than N days (0 = keep forever) — see [Audit log](#audit-log) |
| `--secret-key-file`     | —           | File holding the 32-byte key that seals stored credentials; or env `AGENTS_SECRET_KEY` — see [Secret handling](#secret-handling) |

### Deployment

Standing alone on localhost, no flags are needed. Behind a TLS-terminating
reverse proxy, two things change:

- **`--base-url` is required for OAuth flows** (MCP server OAuth today). Every
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

## Authentication

REST requests authenticate with a Bearer token in the `Authorization` header:

- `Authorization: Bearer <token>`

The `?token=<token>` query parameter is no longer accepted for REST — it leaked
into browser history and proxy logs. The WebSocket instead authenticates at the
application level, via its first message (see [WebSocket protocol](#websocket-protocol)),
resolved by the same credential check as REST.

The auth surface under `/api/v1/auth`:

| Method | Path                             | Auth | Description                                              |
|--------|----------------------------------|------|----------------------------------------------------------|
| GET    | `/auth/config`                   | none | How to authenticate: `{mode, providers?}` — the login page renders from it |
| POST   | `/auth/login`                    | none | Validate the static token (token mode; 400 in OAuth mode) |
| GET    | `/auth/check`                    | yes  | The SPA's stored-credential probe: `{ok}` for a valid Bearer, `401` otherwise |
| GET    | `/auth/oauth/:provider/start`    | none | 302 into the provider's authorize flow (PKCE); sets the login cookie |
| GET    | `/auth/oauth/:provider/callback` | none | Provider redirect target; 302 into the SPA with `#auth_code=<one-time>` on success, `#auth_error=<tag>` on failure (`state_mismatch`, `cancelled`, `exchange_failed`, `not_allowed`, `login_failed`) |
| POST   | `/auth/exchange`                 | none | Trade the one-time code for `{token, user}` — the only response the session token's plaintext rides |
| GET    | `/auth/me`                       | yes  | The authenticated caller: `{id, email, name?, role, avatar_url?}` |
| POST   | `/auth/logout`                   | yes  | Revoke the presented session token (no-op in token mode) |
| GET    | `/auth/audit`                    | admin | The audit log, newest first (`?limit` ≤ 500, `?before=<event id>`) |
| GET    | `/auth/users`                    | admin | Every account with its role and `disabled_at`            |
| PATCH  | `/auth/users/:id`                | admin | `{role?: admin\|member, disabled?: bool}`; never one's own account, never the local one; `409` when it would leave no enabled admin; disabling also revokes every token |
| DELETE | `/auth/users/:id/tokens`         | admin | Sign the account out everywhere: every session and PAT revoked, live connections closed |

### OAuth mode

`--auth oauth` replaces the single static token with per-user Google sign-in
and database-backed credentials:

```bash
./agents-server --auth oauth --base-url https://agents.example.com \
  --oauth-google-client-id XXX.apps.googleusercontent.com \
  --oauth-google-client-secret '...' \
  --allowed-domains example.com
```

- **Admission is an explicit allowlist** — `--allowed-domains` and/or
  `--allowed-emails` (matched against the provider-verified email, lowercased);
  starting with none configured is a startup error, never allow-everyone. A
  domain with an `@` in it, or an address without one, is a startup error too.
- **Who is the admin is decided one of two ways.** `--bootstrap-admin <email>`
  names them: implicitly admitted, admin on every login (the recovery hatch),
  and with it set nobody else becomes admin by signing in. Without it, the
  first account created is the admin and later ones are members — a race
  anyone on an allowed domain can win, which the server warns about at
  startup while no account exists. (Two people's simultaneous first logins
  are serialized, so there is one first.)
- **Logins with the same verified email merge into one account** across
  providers; the (provider, subject) identity is the primary key of a login.
- **The provider's picture URL rides `/auth/me` as `avatar_url`** and the
  browser loads it; the CSP's `img-src` admits each configured provider's
  picture hosts (Google: `https://*.googleusercontent.com`) and nothing else.
  No picture shows initials.
- **A login belongs to the browser that started it.** `start` sets one
  HttpOnly cookie (`__Host-agents_oauth` on https, `SameSite=Lax`, ten
  minutes) holding a nonce the pending login remembers; the callback must
  present it, or the `state` — valid as it may be — is a mismatch. Without
  that, an insider's own callback URL opened in a colleague's browser would
  sign the colleague into the insider's account. The cookie is the login
  flow's alone: API requests authenticate by Bearer, never by cookie.
- **Sessions are rows, not JWTs**: a 30-day sliding expiry under a 90-day
  ceiling no amount of use extends, revoked by `/auth/logout`, cleaned hourly.
  The callback hands the SPA a one-time code in the URL fragment; the session
  token itself never appears in a URL.
- **Off-boarding is a switch, not a deletion.** `PATCH /auth/users/:id
  {disabled: true}` keeps the account and everything it owns but refuses
  every credential of theirs — sessions, PATs, a fresh OAuth login
  (`#auth_error=disabled`) — and revokes what they hold; `{disabled: false}`
  lets them back in. `DELETE /auth/users/:id/tokens` signs one out everywhere
  without disabling. Neither the local account nor one's own account is
  managed this way, and the change that would leave no enabled admin is
  refused — `--bootstrap-admin` remains the recovery hatch.
- **A revocation reaches live connections.** REST authenticates every
  request; a WebSocket authenticates once, so each inbound frame resolves
  its credential again before acting — a revoked token, an expired session
  or a changed role closes the connection (close code 1008) instead, and
  the reconnect's auth frame decides afresh. Signing out, revoking a PAT
  and changing a role also close the user's connections outright (the web
  terminal included); a still-valid credential reconnects and carries on.
  The UI fetches `/auth/me` once at app level and again whenever the socket
  reconnects, since that close is how a changed role reaches it.
- **Where the browser keeps the token.** In OAuth mode the session token
  lives in `localStorage`, so every tab shares the 30-day session and a
  sign-out in one tab reloads the others; token mode keeps the static token
  in `sessionStorage`, gone with the tab. An OAuth sign-in started from a
  deep link (`#/session/…`) returns to that view after the code exchange.
- **`--token` is refused in OAuth mode** — programmatic access uses personal
  access tokens instead (see below). The Google redirect URI to register is
  `<base-url>/api/v1/auth/oauth/google/callback`.
- The client secret can come from `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET` instead
  of the flag. Secrets configure the process only; they are never stored in
  the database.

**Personal access tokens** are OAuth mode's programmatic credential (curl,
scripts, CI): `POST /auth/tokens {name, expires_in_days?}` answers with the
plaintext exactly once (`ags_p_…`, 0 days = never expires); `GET /auth/tokens`
lists labels and dates, never secrets; `DELETE /auth/tokens/:id` revokes. A
PAT authenticates everywhere a session token does — REST and the WS auth
frame. In token mode the endpoints answer 400: the static compare is the whole
check there, so a PAT could be minted but never authenticate.

### Ownership and roles

Two rules, enforced at the routes (`handler/authz.go`), shape who may do what:

- **Shared configuration is read by every member and written by admins
  only.** Agents, providers, MCP servers, sandboxes (the test endpoint
  included), settings, skill repos, workflows, guardrails, memories and
  provider routes: every write changes what runs on the host or whose
  credentials are spent, so `POST`/`PUT`/`DELETE` on them answer `403` for a
  member. The web terminal (`/ws/terminal`) is a shell on a sandbox host with
  the server's stored credentials — admin only. The gate holds through the
  model's tools as well: `save_workflow` rides only an admin's run (a member's
  agent still gets `get_workflow`), so a member approving their own agent's
  call cannot write what the API refuses them.
- **Using shared configuration is not writing it.** A member runs any agent,
  any workflow, and any sandbox in their own session — and approves their own
  tool calls there. A shared `ssh` or `local` sandbox is therefore a shared
  shell: every member who can pick it can execute on that host, under the
  credentials the server stores. That is the single-workspace model — one
  team, one trust boundary — not an oversight; a host that only admins may
  touch is a host that is not configured as a sandbox here.
- **A session's content belongs to its owner alone.** `sessions.owner_id` is
  the one ownership column: a task's hidden session inherits its parent's
  owner, a trigger fires into a session, an approval is filed on one. The
  `/sessions/:id`, `/runs/:id`, `/tasks/:id`, `/approvals/:tool_call_id` and
  `/triggers/:id` subtrees are gated on owning that session, and a foreign id
  answers `404` — the same as a missing one, so ownership is not an oracle for
  existence. Listings (`/sessions`, `/tasks`, `/triggers`) are the caller's.
  Running a workflow or creating a trigger is into a session the caller owns.
  Run events over the WebSocket reach the owner's connections only: the
  broadcast bus is per owner.
- **An admin manages, never reads.** `GET /sessions?all=true` lists every
  owner's sessions (existence and recency), and `DELETE /sessions/:id` works on
  any of them; opening, reading or running one is the owner's alone. Roles are
  `admin` and `member`: the first OAuth account and `--bootstrap-admin` sign
  in as admin. In the UI the account menu (sidebar footer: avatar and name)
  holds Settings, Sign out and — for admins — Admin, a dialog of three
  panels: Members (roles, disabling, signing out everywhere), Sessions
  (every owner's: reassign or delete, never read) and Audit logs. Settings for a member shows the same configuration panels read-only
  — what the API lets them read, laid out as the admin sees it, with no
  Add, Edit, Delete or Test — plus their Account (profile and PATs). The
  Terminal button is shown to admins only, as `/ws/terminal` is admin-only
  server-side; nothing else in the UI hides what the server would allow.

In token mode the one local account is an admin and owns everything, so every
check passes.

**Switching auth modes keeps the data and changes who can reach it.** Every
session made in token mode belongs to the local account, which OAuth mode
leaves dormant; every session made in OAuth mode belongs to the person who
made it, whom token mode cannot sign in as. After a switch the old sessions
are therefore visible only in the admin's `GET /sessions?all=true` listing
(and the Admin dialog's Sessions panel), where an admin can reassign one to
an account with `PUT /sessions/:id/owner` — or delete it.

### Audit log

`audit_events` answers "who did what, to what, when". Two shapes of line:

- **A request.** Every successful mutating API request (`POST`/`PUT`/`PATCH`/
  `DELETE` under `/api/v1` answering below 300) leaves `METHOD /route/pattern`
  as the action, the caller from the credential, and as the resource the
  path parameter or — for a create, which has none — the id of what was
  created. A handler annotates a short detail where the route alone cannot
  say what happened: `role=member disabled=true`, `owner=<id>`, and on an
  approval the verdict, the scope and the tool (`approve scope=all
  tool=exec_command` is the one to notice). Never a request body, never a
  secret. Failures and reads leave nothing. The line is written on its own
  goroutine after the response.
- **An act that is not a request**, named by what it is:

  | Action          | Who is the actor                        | Resource          | Detail                          |
  |-----------------|-----------------------------------------|-------------------|---------------------------------|
  | `ws.run.create` | the connection's user                   | session id        |                                 |
  | `ws.approval`   | the connection's user                   | tool call id      | verdict, scope, tool            |
  | `terminal.open` | the connection's user                   | sandbox id        |                                 |
  | `workflow.save` | the session's owner (who approved it)   | workflow id       | `tool=save_workflow created`    |
  | `trigger.fire`  | the owner of the session it fired into  | trigger id        | `source=cron\|webhook started=` |
  | `POST /auth/login`, `POST /auth/exchange` | the account that signed in | | |

  A person's manual fire (`POST /triggers/:id/fire`) is the request's line;
  the clock's and a webhook's have no request, so the scheduler writes
  theirs. A `save_workflow` is the one write to shared configuration that
  happens through a tool, so the tool writes it — the approval line alone
  would read like any other `exec_command`.

Retention is the process's `--audit-retention-days` (default 0 = keep
forever), deliberately not a setting: the log of configuration changes must
not be shortened through the API it records. The log carries each actor's
email and client IP — personal data; the flag is its retention control, and
deleting the database file deletes the log with it. Admins read it at
`GET /auth/audit` (`?limit=` up to 500, `&before=<event id>` pages older —
the id is a UUIDv7, so it orders like the time and never ties) and in the
Admin dialog's Audit logs.

Exempt from auth: the MCP OAuth redirect callback
(`GET /api/v1/mcp-servers/oauth/callback` — the browser follows it without an
Authorization header), the OpenAPI document (`GET /api/v1/openapi.yaml`), the
`config`/`login`/`exchange` and `oauth/*` endpoints above, and
`GET /health`. Every entry on that
list must name a route this router actually serves: an exemption for a path
nothing serves silently unauthenticates whatever gets mounted there later. The
converse holds for the redirect URI the OAuth handler hands the authorization
server — the handler builds it from `server.APIPrefix` and the one
`mcpOAuthCallbackPath` constant its route is mounted on; `bridge` receives the
finished URI and knows no path. A callback that lands anywhere else under
`/api/` is neither routed nor exempt, so every login ends in `401 unauthorized`. The
ChatGPT login callback is deliberately absent — its redirect lands on a
temporary listener at 127.0.0.1:1455, never on this server. Webhook triggers
(`POST /hooks/:id`) live outside `/api/` for the same reason a callback does —
the caller is another system, with no token — and are authenticated by HMAC
signature instead (see [Workflows](#workflows--apiv1workflows)).

## REST API

Base path `/api/v1` — the only mount; there is no unversioned alias. All
request and response bodies are JSON. Request bodies are capped at 1 MiB
(matching the WebSocket frame limit): a declared length past it answers
`413`, an undeclared body is cut there and fails its decode. One route
carries more — `POST /playground/generate` replays a stored span payload,
so its cap is the `trace_span_data_kb` setting plus 256 KB.

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
| `unavailable`  | 503  | Transient refusal while the server drains for shutdown — retry later          |
| `rate_limited` | 429  | This client IP exceeded an endpoint's rate budget — slow down and retry       |

### Response conventions

- **Create** returns `201 Created` with the created resource.
- **Update** (`PUT`/`PATCH`) returns `200 OK` with the full updated resource.
- **Delete** returns `204 No Content`.
- A write (update or delete) against a missing resource returns `404`.
- Secret fields are write-only — see [Secret handling](#secret-handling).

### Sessions — `/api/v1/sessions`

| Method | Path                      | Description                                                          |
|--------|---------------------------|----------------------------------------------------------------------|
| GET    | `/sessions`               | List sessions — the caller's; `?all=true` every owner's (admin) |
| POST   | `/sessions`               | Create session (`{name?, agent_config_id?}`)                         |
| GET    | `/sessions/:id`           | Get session — plus `planning`, whether its next run starts by planning |
| PATCH  | `/sessions/:id`           | Partial update — `{name?, pinned?}`, returns the updated session |
| DELETE | `/sessions/:id`           | Delete the session and everything it owns: entries, traces, approvals, wake-ups, triggers, and its task tree (rows and hidden child sessions, at any depth over live edges) |
| PUT    | `/sessions/:id/owner`     | Reassign the session and its task tree to `{user_id}` (admin); `409` while a run is live on it |
| GET    | `/sessions/:id/messages`  | List session entries (paginated)                                     |
| GET    | `/sessions/:id/traces`    | List trace events (paginated); `?summary=true` leaves each span's payload fields out (`payload_omitted`) — what the trace panel opens with |
| GET    | `/sessions/:id/traces/:span_id` | One span whole, payload included — what a summary row opens with, or a live span the WebSocket cap trimmed |
| GET    | `/sessions/:id/runs`      | Every run that left entries, oldest first — `{run_id, question, on_path}`: the user text it started from (a regenerate inherits the message it answered again) and whether it is on the active branch |
| GET    | `/sessions/:id/context`   | Context-window usage report (see [invariant 28](#design-invariants)) |
| POST   | `/sessions/:id/compact`   | Force one compaction pass now (409 while a run is live)              |
| POST   | `/sessions/:id/fork`      | Fork session into a new one                                          |
| POST   | `/sessions/:id/branch`    | Switch the active branch (`{entry_id}`)                              |
| POST   | `/sessions/:id/runs`      | Start a run on the session (see [Runs](#runs--apiv1runs))            |
| GET    | `/sessions/:id/approvals` | List pending approvals (see [Approvals](#approvals--apiv1approvals)) |
| GET    | `/sessions/:id/tasks`     | List background tasks spawned from the session (see [Tasks](#tasks--apiv1tasks)) |

`POST /sessions` accepts an optional `agent_config_id` to bind the session to an
agent at creation (it must reference an existing agent). Rename and pin are a
single `PATCH /sessions/:id` accepting a partial `{name?, pinned?}` body; both
the separate `PUT` rename and `PATCH /sessions/:id/pin` endpoints are gone.

Session responses also carry `sandbox_id?` / `work_dir?` — the session's
sandbox binding, written by its first sandbox-carrying run (see
[Runs](#runs--apiv1runs)); absent while unbound. The binding is **immutable**:
neither half can be set or changed over the API — one conversation, one file
system context. Switching projects means starting (or forking into) another
session; the composer's Project picker is that flow in the UI.

`fork` copies the source session's entries (and their traces) into a new
session. Its body is optional: `{message_id?, exclusive?, label?}`. Omit
`message_id` to fork everything; supply it to bound the copy up to and including
that entry (`exclusive: true` excludes the boundary entry itself). Entry ids and
their parent links are rewritten into the fork's namespace, so the copy is a
self-consistent tree rather than one pointing back at another session. The
session inherits the source's `agent_config_id` and its sandbox binding
(`sandbox_id` / `work_dir`) — a fork continues the same conversation over the
same file system context, with no fresh bind of its own.

`branch` moves the session's active branch to an entry, so the next run
continues from there. It APPENDS a leaf entry rather than deleting anything:
the abandoned attempt stays recorded, which is what makes "regenerate" show a
"2 / 3 ‹ ›" switcher instead of filling the session list with `(regen 2)`,
`(regen 3)` copies. Each entry reports `on_path` — false means an abandoned
attempt, still stored and still switchable-to.

Regenerating is `branch` back to the user's message followed by a run with an
EMPTY input: nothing to add, history to answer.

`/sessions/:id/messages` returns **session entries** — the SDK's
`agents.SessionEntry` as the runner wrote it, plus the row id the cursor pages
on. Each carries its `kind` (`item` / `annotation` / `compaction` / …), its
recorded `display`, and its `usage` / `diagnostics`. Update entries are folded
into their targets server-side, so a client never applies them itself. The path
keeps its name for compatibility. The UI loads the newest 200 and pages
backwards on demand ("Load earlier messages"), while `/sessions/:id/traces`
loads the whole session's spans at once — so the trace panel labels a card
whose exchange is not on screen from `/sessions/:id/runs`, the server's own
walk over every entry, rather than showing a bare run id.

`/sessions/:id/context` reports what the session's ACTIVE branch occupies of its
model's context window — the Context panel's whole payload, recomputed per call
from the entries (there is no live event for it; the panel refetches when a run
ends). `input_tokens` is the LAST model call's input: the history, prompt and
tool schemas in the window right now — not `session_input_tokens`, which totals
every call and so counts re-sent history once per turn. `context_window` is the
agent config's declared window (`provider.context_window`; 0 when unset, and the
panel then shows occupancy without a denominator) and `conversation_tokens` is
the estimated size of the transcript still in context — every active,
uncompacted entry summed, the row the "In the window" section shows beside the
prompt layers. Compacted and off-path entries keep their usage — the call
happened — but leave `conversation_tokens` and `compaction_tokens`, because the
model no longer sees them.

The report costs the session's ROW COUNT, not its size: `entries` carries the
usage and the character estimate of each entry as lifted columns (written by
the append that stored it), so the endpoint reads no entry bodies beyond the
branch walk's leaf markers. Without the columns a report over a 234MB session
took ~1.4s and re-read every byte; with them it is ~30ms. The compaction check
on every append reads the same columns.

`prompt` is what the session's last build put in front of the conversation: the
instruction layers (`instructions` / `global_prompt` / `memory` /
`skills_index`) and the tool surface bucketed by origin, all in CHARACTERS. It
comes from the `context_profiles` snapshot the runner writes per run — only the
build knows what it assembled, and rebuilding it in a read path would be a
second copy of `buildAgentFromConfig`. MCP is the exception: its tools live on
the server, not on the agent, so the read path asks each connected server
(bounded, 2s) and reports one that cannot answer as `unavailable` rather than
zero. Absent entirely until a run has built the session's agent once.

`POST /sessions/:id/compact` forces one compaction pass right now — the
panel's "Compact now". It reuses the run path's own construction (same
summary-model resolution, same adapter), and `Force` skips only the threshold:
the kept window, pairing-safe split and summary-of-summary guards all still
apply, so the worst outcome is a 200 with `compacted: false` (nothing to
fold). 409 while a run is executing — the run compacts at its own boundaries;
400 when the session's agent has compaction disabled or no usable provider.

**Pagination** — `messages` and `traces` accept optional `?limit=` and
`?before_id=`. Without `limit` the full list is returned (oldest-first),
backward-compatible with older clients. With `limit`, the newest `limit` items
are returned; page backwards by passing the smallest id you received as
`before_id` (an exclusive upper bound). Row ids are UUIDv7 strings and order
by insertion — `NewV7` is monotonic within a process — so "smallest" is the
first one in a page. For `messages` the limit counts the
ENTRIES a client receives, not table rows — update entries are folded into
their targets first, so a page is never short of what was asked for. The web UI
loads the newest 200 and offers "Load earlier messages".

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
| POST   | `/sessions/:id/runs` | Start a run — `{input, agent_config_id?, sandbox_id?, work_dir?}`                  |
| GET    | `/runs/:id`          | Get run status                                                                     |
| GET    | `/runs/:id/events`   | Stream run events (Server-Sent Events)                                             |
| POST   | `/runs/:id/cancel`   | Cancel the run — `204`; `?mode=graceful` finishes the current turn, default aborts |

`POST /sessions/:id/runs` returns `201` with `{run_id, session_id, status}`. With
the header `Prefer: wait=N` ([RFC 7240](https://www.rfc-editor.org/rfc/rfc7240))
it holds the request up to N seconds and returns `200` with
`{run_id, session_id, status, final_output}` when the run ends in time — or,
when the run pauses for tool approval, `{run_id, session_id, status:
"interrupted"}` (list `/sessions/:id/approvals` and decide; the decision resumes
execution on the SAME run id, continuing its event sequence). When N passes
first it returns `202` with `{run_id, session_id, status: "running"}` and the
run keeps going — follow it on `/runs/:id/events`. `Preference-Applied: wait=N`
marks the honored wait; there is no unbounded form, and N is capped at ten
minutes (`MaxPreferWait`) — a longer wait is the events stream's job. It returns `409` if the
session already has an active run.

The first run that carries a `sandbox_id` **permanently binds**
`(sandbox_id, work_dir)` to the session (compare-and-set; the winner announces
it with a `session.sandbox_bound` event). From then on the server uses the
bound values and ignores whatever the client sends — the conversation's file
system context never changes. Runs without a sandbox never bind, so a
chat-only session can still pick one later.

A new binding is **validated before it is written**, and only after the run
has been accepted (a run refused as busy/deleting/draining binds nothing).
The sandbox must exist, and `work_dir` must be one its backend honors — the
canonical form is what gets stored:

- `local` — empty (the server `--workspace`) or an absolute path.
- `ssh` — a fixed **absolute** directory is required, either here or as the
  config's default `work_dir`: without one every exec runs in a throw-away
  remote temp dir, and a relative one resolves against a login home the
  remote host can move — either way the binding would not say where the
  session's files are.
- `docker` persistent — empty, `/workspace`, or a `/workspace` subtree (the
  mount never moves; the session works in a subtree of it).
- `docker` ephemeral — empty only (each exec always runs in `/workspace`).

A request that fails validation is `400` and leaves the session unbound.

`GET /runs/:id` returns `{run_id, session_id, status, last_seq, agent_config_id?,
sandbox_id?, work_dir?}`. `status` is one of `running`, `interrupted`, `completed`, `error`,
or `cancelled`. Finished runs stay queryable and replayable for **15 minutes**
after they end, then `GET /runs/:id` returns 404 (the conversation itself is
always in `/sessions/:id/messages`).

`GET /runs/:id/events` is a Server-Sent Events stream. (This is plain HTTP SSE
for API consumers — unrelated to MCP's deprecated SSE transport, which this
server does not expose.) Each event's `id:` is the hub sequence number;
reconnect with the `Last-Event-ID` header (or `?from_seq=`) to resume without
losing events. The stream closes after a FINAL event — `run.output`,
`run.error` or `run.cancelled`. `run.interrupted` (paused for approval) does
NOT close a live stream: the approval decision resumes the SAME run id, and
the resumed events continue on the connection you are already holding. A
client that did disconnect reconnects with its `Last-Event-ID` and picks the
sequence back up.
Event payloads mirror the WebSocket [server→client events](#server--client).

Start a run and stream it with plain curl (token from server startup):

```bash
TOKEN=...; H="Authorization: Bearer $TOKEN"; BASE=http://127.0.0.1:9527/api/v1
SID=$(curl -s -H "$H" -X POST $BASE/sessions -d '{"name":"cli"}' | jq -r .id)
RUN=$(curl -s -H "$H" -X POST $BASE/sessions/$SID/runs \
      -d '{"input":"hello","agent_config_id":"<agent-id>"}' | jq -r .run_id)
curl -N -H "$H" $BASE/runs/$RUN/events          # stream until run.output

# or fire-and-wait in one call, for up to a minute:
curl -s -H "$H" -H "Prefer: wait=60" -X POST $BASE/sessions/$SID/runs \
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

### Tasks — `/api/v1/tasks`

A task is one piece of background work started from a chat through the ONE
tool that starts any: `spawn_task` — a sub-agent on a prompt, or, told a
`workflow` name, a workflow execution (`kind: "workflow"`, see
[Workflows](#workflows--apiv1workflows)).
Each runs on its own hidden session and reports back by injecting a
notification into the parent session (see the SDK's
[background tasks](../../docs/tasks.md)).

| Method | Path                  | Description                                                                    |
|--------|-----------------------|--------------------------------------------------------------------------------|
| GET    | `/sessions/:id/tasks` | List the session's tasks and workflow executions, newest first                 |
| GET    | `/tasks`              | One page (`{items, total}`) of tasks across every live session, newest first, each with its `session_name` — `?kind=workflow` for executions, `?live=true` for the still-running, `?limit=` (500 at most) and `?offset=` cut the page |
| POST   | `/tasks/:id/stop`     | Stop a task — cancels a running one, discards a paused one's pending approval; a workflow's whole sequence, not just the step |
| POST   | `/tasks/:id/retry`    | Resume a FAILED task: same session and progress, a new run — a workflow from the step it stopped at |
| POST   | `/tasks/:id/dismiss`  | Hide a terminal task from the chat strip (the panel keeps it; a retry brings it back); 409 while it runs |

A task row carries `task_id` (the durable identity clients key state by),
`run_id` (the current run's execution id — events route by it; a retry
replaces it, and so does each workflow step), plus `parent_session_id`,
`parent_run_id`, `tool_call_id`, `label`, `kind`, `attempt` (1 for the
original run, one more per retry), `dismissed` and status; a workflow's row
also carries `state` — the definition snapshot, the current `step_id` and the
`step_runs` launch log. Status uses the MCP Tasks five-state vocabulary; it is
read live from the hub for a running task and from the store after it ends.
`stop` returns `200` with the task info, `409` if the task is already final.
`retry` returns `200` with the reopened task, `409` when the task is not
failed, has used every attempt (3 by default), or its session is at the
live-task cap — which is the `--max-tasks` flag, and which a retry queues
behind like a spawn.

### Agents — `/api/v1/agents`

| Method | Path                         | Description                              |
|--------|------------------------------|------------------------------------------|
| GET    | `/agents`                    | List agent configs                       |
| POST   | `/agents`                    | Create agent                             |
| GET    | `/agents/:id`                | Get agent                                |
| PUT    | `/agents/:id`                | Update agent                             |
| DELETE | `/agents/:id`                | Delete agent                             |
| GET    | `/agents/:id/tools`          | The agent's current tool surface as schema-only definitions (`{name, description?, parameters?}`) — built-ins, connected MCP servers' tools, the skills reader; sandbox tools excluded (no sandbox is selected). Nothing here executes; backs the Replay dialog's tool picker |

Agent config shape — three top-level scalars, then the knobs as **grouped
nested objects** (each group is one JSON column in the table, so a new knob
needs no schema change), then a few top-level JSON blobs:

- **Top level**: `name`, `instructions`, `model`, `provider_id` (the endpoint
  this agent reaches its model through — see [providers](#providers--apiv1providers);
  empty means the built-in default, the openai backend on the global api-key
  setting), `context_window` (declared, 0 = unknown)
- **`behavior`**: `max_turns`, `handoff_description`, `disable_tool_choice_reset`,
  `stop_at_tools` (comma-separated tool names — the run ends after a turn that
  called any of them), `handoff_input_filter`, `max_tool_concurrency`,
  `tool_not_found_behavior` (unset feeds a tool name the agent does not have
  back to the model so it can correct itself; `error` ends the run instead),
  `reasoning_item_id_policy` (`preserve` / `omit`), `workflow_authoring`
  (gives the agent's chat runs `get_workflow` / `save_workflow`, every save
  approved — see [workflows](#workflows--apiv1workflows))

  Plan and todo mode are NOT here. `todo_write` is on every chat agent — when a
  job is worth tracking is the model's judgement, like any other tool. Plan mode
  is a restraint, so it belongs to the session and the person: it rides on the
  run request (`plan`), and the session reports it as `planning`. Workflow
  authoring IS here, and off by default, for two reasons that do not apply to
  todo: its save schema rides on every request of the agent that carries it,
  and writing definitions is one agent's job — the builder's — not something
  every coding agent should be offered ([invariant 39](#design-invariants)).
- **`resilience`**: `retry_enabled`, `retry_policy`, `fallback_models` (JSON
  array of `{model, provider_type, api_key, base_url}`; `provider_type`
  defaults to `openai`, and unknown keys are rejected)
- **`guardrails`**: `guardrails` (JSON array of names — one list, since a
  guardrail carries the stages it inspects), `output_schema` (JSON Schema)
- **`session`**: `prompt_id`, `prompt_version` (OpenAI stored prompt),
  `history_limit` (recent items per turn; `0` = all)
- **`approval`**: `approve_tools` (JSON array: `["*"]` or tool names — the
  human-in-the-loop gate; the `exec_command` approval flow above depends on it)
- **`compaction`**: `compaction_enabled`, `compaction_threshold_tokens` (a
  pass fires when the active history sizes past this many tokens, priced from
  the newest entry's real usage plus a byte estimate of what follows; `0` =
  50000 — a NEW key: the retired `compaction_threshold` counted entries, and
  a stored value silently reinterpreted as tokens would compact every turn),
  `compaction_window` (recent ENTRIES kept intact; `0` = 10),
  `compaction_model`, `compaction_prompt`. With compaction enabled, a
  context-overflow error from the provider also triggers a FORCED pass and the
  turn retries from the shrunk history (SDK overflow recovery, spec §2.5g) —
  the threshold predicts, this reacts.
- **Top-level JSON blobs**: `model_settings`, `tools`, `skills`, `handoffs`,
  `error_handlers` (keyed by `max_turns` / `model_refusal` /
  `invalid_final_output`; each entry is `{"final_output": <JSON value>,
  "exclude_from_history": bool}` — the run completes with the static fallback
  instead of failing; `final_output` must be a string for plain-text agents or
  match `output_schema`)

```json
{
  "name": "coder",
  "model": "gpt-5.2",
  "provider_id": "9f2c…",
  "behavior": {"max_turns": 20},
  "approval": {"approve_tools": "[\"exec_command\"]"}
}
```

An agent body carries no model-API credential at all — it names a provider,
which is where the key lives (the ChatGPT OAuth flow included — see
[providers](#providers--apiv1providers)). What is still masked here is each
`resilience.fallback_models[].api_key` (see [Secret handling](#secret-handling)).

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

The one transport is streamable HTTP (spec §5.25) — `config` is `{endpoint,
headers, auth_mode, oauth_*}` with `auth_mode` `header` or `oauth`; a local
stdio-only MCP server can join through a stdio→HTTP proxy such as `mcp-proxy`.
Enabled servers are connected automatically on
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

The interactive flow logs its progress, so a stuck `authorizing` is diagnosable
from the server log. `authorization URL issued` records the exact `redirect_uri`
the authorization server must send the browser back to; a completing login then
logs `callback: authorization code delivered` followed by `interactive connect
established`. Two distinct failures both surface as a stuck button, told apart by
which line is missing:

- **No callback line at all** (only the panel's `GET /mcp-servers` poll repeats):
  the browser never reached the callback. The authorization server rejected the
  `redirect_uri` — a pre-registered `oauth_client_id` whose allowed callback does
  not list this exact path — or the browser cannot reach this origin (a reverse
  proxy or non-loopback host, so `redirect_uri` resolves elsewhere;
  `externalOrigin` derives it from the request's `Forwarded` / `X-Forwarded-*`
  headers, then the direct host). A callback that arrives but cannot be matched
  logs `callback: could not deliver authorization code` with the reason.
- **`code delivered`, then `ended without connecting` with `authorization
  completed but was not accepted`**: the browser round-trip worked, but the
  authorization did not yield a working session, so the SDK re-authorized
  mid-connect; the interactive park is single-shot — the frontend opened one
  popup, and there is no second one to service — so the attempt fails fast
  rather than hanging until the 5-minute timeout. `has_oauth_token` splits the
  cause: still false means the SDK rejected the authorization response before
  any token exchange — typically AS metadata inconsistent with the authorize
  redirect (RFC 9207: `iss` arrives but the metadata does not advertise
  `authorization_response_iss_parameter_supported`, or the advertised `issuer`
  differs from the `iss` received — common when a gateway proxies a real IdP's
  endpoints under its own issuer). True means a token was issued and persisted
  but the resource server rejected it — set the server's `oauth_scopes` to what
  it requires, or confirm the token's audience is this MCP endpoint. The first
  case is a server-side metadata bug: its metadata must present the issuer
  exactly as the IdP responds, or its PRM should point `authorization_servers`
  at the IdP directly.

OAuth grants obtained during authorization are persisted — the token together
with the token endpoint and (possibly dynamically registered) client
credentials — and reported as `has_oauth_token`, so reconnecting — including
the automatic reconnect after a disable/enable cycle or a restart — needs no
re-authorization. Expired access tokens are refreshed automatically, both
mid-session and when reconnecting after a restart; every refresh (including a
rotated refresh token) is written back to the store. Only when the refresh
token itself is rejected does the server fall back to interactive
authorization: in-flight tool calls fail fast with a re-authorize message
rather than hanging, and the next connect returns an authorize URL. Use the
`oauth-token` DELETE endpoint — the "Clear auth" button in the server's edit
form — to drop the saved grant, e.g. to re-authorize with a different account.

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

| Method | Path             | Description                                      |
|--------|------------------|--------------------------------------------------|
| GET    | `/settings`      | List all stored key-value pairs                  |
| GET    | `/settings/:key` | Get value                                        |
| PUT    | `/settings/:key` | Set value (400 on an unknown key or a bad value) |
| DELETE | `/settings/:key` | Delete                                           |
| GET    | `/setting-defs`  | The registry: every key, its kind and default    |

The keys, their types, defaults and how the panel presents them all come from
ONE table — `internal/settings`'s registry, served at `/setting-defs`. Nothing
here is a second source of truth: the backend reads a key through it, secret
masking derives from it, and the panel renders from it. Adding a global setting
is one entry there.

A write names a defined key and carries a value its kind accepts, or it is a
`400`: `trace_span_data_kb: "abc"` used to be stored and then silently ignored
at read time, and a mistyped key used to become a row nothing would ever read.
An EMPTY value is always accepted — that is how a setting is returned to its
default. Reads are laxer than writes on purpose: `GET /settings` lists a key the
registry no longer defines with `"unknown": true`, and `DELETE` takes it, so a
value left behind by an older build can be seen and cleared rather than being
hidden with no way to remove it.

Known keys:

- `proxy_url` — HTTP proxy for model and MCP calls
- `system_prompt` — global system prompt prefix
- `openai_api_key`, `anthropic_api_key` — fallback keys, one per backend, used
  by a provider row (or a `fallback_models` entry) that carries none of its own
  **and no custom `base_url`** — a key must not follow a redirect to an
  arbitrary endpoint, so a row pointing elsewhere needs its own key — and by an
  agent with no `provider_id` at all (secret; masked on read — see
  [Secret handling](#secret-handling))
- `brave_api_key` — injects a `brave_search` tool into all agents (secret; masked
  on read — see [Secret handling](#secret-handling))
- `trace_retention_days` — prune trace events older than N days (checked at
  startup and once a day); default 30, `0` keeps everything. Each generation
  span stores the whole conversation it was given, so the table grows with
  the square of a session's length — keeping everything is a choice
- `trace_include_sensitive_data` — `false` keeps prompts, outputs and tool
  arguments out of stored traces (generation spans carry only timing/usage
  metadata; the trace panel's Replay then has nothing to seed from). Empty or
  `true` records everything (the default). Applies to new runs
- `trace_span_data_kb` — how much of a span's payload (model request, response,
  tool schemas) is STORED, in kilobytes; empty or `0` uses the default 8192.
  Past it the bulky fields are replaced with a marker and a Replay of that call
  has nothing to seed from, so raise it if you replay large turns and can pay
  the disk (a 74k-token request is roughly 300KB–1MB per generation span;
  `trace_retention_days` is the other half of that budget). What travels over
  the WEBSOCKET is a separate, fixed 256KB — the browser holds every span of
  the session at once, and anything it drops (`payload_omitted`) is still in
  the row, which the panel fetches when the span is opened. Applies to new runs
- `log_sensitive_data` — include prompts, tool arguments and model output in
  the SDK's own log records. Deliberately separate from
  `trace_include_sensitive_data`: traces go into the database, logs go to
  stderr and whatever collects it, so they are different decisions. Off by
  default, and visible only at `--log-level debug`. Applies to new runs
- `approval_ttl_minutes` — how long a pending tool approval may sit unanswered
  before it expires (default `1440` = 24h; `0` disables expiry)
- `max_terminals_per_sandbox` — concurrent interactive terminals allowed on one
  sandbox (default `4`, max `32`) — a fat-finger guard, not a scheduler

### Server info — `/api/v1/server` (read-only)

| Method | Path      | Description                              |
|--------|-----------|------------------------------------------|
| GET    | `/server` | The start-up configuration now in force  |

`{version, workspace, allow_local_sandbox, max_tasks}` — the flags this process
was started with, not settings. They are here because a client that cannot see
them meets them only as unexplained refusals: "local sandboxes are not allowed"
with nowhere to learn that a flag decides it. `workspace` is absolute (`.`
means nothing to a browser elsewhere) and `max_tasks` is the EFFECTIVE cap, not
the raw flag — `--max-tasks 0` means the built-in default, and reporting the
zero would be a lie.

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

### Providers — `/api/v1/providers`

One configured endpoint and the credential that reaches it: `name`, `type`
(`openai` default / `anthropic` — selects the API protocol), `auth_mode`
(`chatgpt_login` is openai-only), `api_key`, `base_url`. Agents REFERENCE a
provider by id; nothing else stores a model-API key, so this
is the one surface a credential crosses (masked on read, `********` keeps the
stored value — but only while `type` and `base_url` are unchanged, since the
stored key belongs to that destination).

The ChatGPT OAuth flow lives here too, for the same reason: the token is this
endpoint's credential, so every agent pointed at the provider shares one login.
`POST /providers/:id/chatgpt/login` · `/logout` · `GET /status`.

| Method | Path              | Description                                  |
|--------|-------------------|----------------------------------------------|
| GET    | `/providers`      | List providers (keys masked)                  |
| POST   | `/providers`      | Create provider                               |
| GET    | `/providers/:id`  | Get provider                                  |
| PUT    | `/providers/:id`  | Update provider                               |
| DELETE | `/providers/:id`  | Delete; 409 while an agent uses it            |

### Workflows — `/api/v1/workflows`

A workflow is a FIXED, ordered sequence of steps run on ONE session. Each step
names the agent that runs it and the prompt that starts its turn, so
plan → exec → verify can be three different agents on three different models —
which is the point. Which step runs next is the definition's answer, not the
model's; a model-chosen next agent is a handoff, and that already exists.

An execution IS a background task — `kind: "workflow"` in the tasks table and
API — whose runs are the steps: the SDK's task manager owns its lifecycle
(the hidden child session, stop, retry, the restart sweep, the approval
pause, the cap, the wake-up) and this server is only the DRIVER the manager
calls back into: which step a finished run leads to, and how a step's run is
launched ([invariant 29](#design-invariants)). There is no second execution
table and no second set of endpoints: an execution is listed, stopped,
retried and dismissed as a task.

A step is an ORDINARY RUN on the execution's session, which is what makes the
sequence cheap: one window, one compaction, one sandbox binding, one
transcript, and a step may use tools or hand off like any other turn — but NOT
spawn background tasks or start another workflow: a step is a task's run, and
those tools are withheld from one (a sequence cannot fan out into more
background work). Later steps read what earlier ones did, with no data
plumbing between them — the conversation is the data flow.

A workflow carries a required `description`: it is what an agent matches a
request against — `spawn_task` lists the workflows on offer, name and
description, and the agent starts one by naming it — and an agent doing so is
how a workflow usually starts (see [invariant 30](#design-invariants)); a
person can start one too, with a brief of their own (`POST /workflows/:id/runs`). A step may set `compact_before`,
folding the conversation into a summary before it runs — with the step's own
agent's compaction settings, since that agent is the one about to read the
summary (an agent whose compaction is off leaves the transcript as it is,
logged): a step boundary is the
natural place for it, since the exploration that got the sequence here is
spent and every later step pays for it otherwise. A step may set
`pause_before`: the sequence holds there until a person approves it from the
conversation that asked ([invariant 37](#design-invariants)) — for the deploy
or the send that must not happen unseen. Rejecting cancels the execution.

A step may also name where to go next — `on_success` and `on_failure`, each a
step id or `end`. Their empty defaults ARE the plain list (success falls through
to the next step, the last one finishes, and a failure fails the workflow), so a
linear workflow never mentions them. Naming an EARLIER step is how a sequence
loops: `test.on_failure = fix`, `fix.on_success = test`. Three rules make that
safe — a **lap bound**: one execution may take the same backward edge
`budget.max_laps` times (default 3), and the transition that would take it
once more ends the execution failed, naming the edge (`loop bound reached:
verify → exec looped 3 times`) — a loop that keeps returning to the same step
is not converging, and every further lap costs a step run for the same
answer; an execution stops after `MaxStepRuns` (50) step launches, retries
included, whatever the shape (a retry past either bound is refused before a
run — one more lap could only end the same way); and a handler's turn is LED
by the error it is handling, because a failed run leaves no usable account of
itself in the transcript.

"Failure" is structural by default: the step's run errored. A step that CHECKS
— tests, a review, a verification — sets `gate`, and then its verdict chooses
the edge instead: the last non-empty line of its output must be the pass
sentinel (`PASS`, or the gate's own word) for `on_success` or the fail
sentinel (`FAIL`) for `on_failure`; the instruction to end with one is appended
to the step's prompt for it. A step whose agent answers in structured output
carries the verdict as a field instead — a JSON object (bare or fenced) with a
boolean `passed`, or a `verdict`/`result`/`status` equal to a sentinel — and
the same routing reads it. A gate that reports neither ends the execution
failed, saying so — a check that forgot to report is a broken step, not a coin
flip — and a `FAIL` with no `on_failure` fails the execution too. Either way
the routing stays the definition's: the step only reports, which is what keeps
a workflow deterministic ([invariant 30](#design-invariants)); with `gate` and
a back-edge, `check → FAIL → fix → check` is the fix loop a sequence exists
for. The launch log (`state.step_runs`) records how each step's run ended —
`completed`, `failed`, `pass`, `fail` — the last one included: the ending is
written in the same finalize as the task's terminal status, so a finished
execution's log needs no reading between the lines and cannot disagree with
its status.

An execution carries an `input` — the brief, written by the agent that started
it, because the child session cannot see the conversation. It LEADS the first
step's turn and is not repeated afterwards: from step two on it is already in
the transcript the step reads. It is kept in the task's `state` so a reader
can see what the sequence was about.

The agent that started one is told the task id, and asks after it with
`task_status(task_id)` like any task — the answer says which step it is on
(`progress: step 2/3 (verify)`), through the SDK's `DescribeState` hook — or
with `task_status()` and no id, which lists every task of the conversation,
each live one flagged "still working — do not redo its work". The model's
whole vocabulary for background WORK is the four task verbs: `spawn_task` (with
`workflow` for a sequence), `task_status`, `task_retry`, `task_stop`; there is
no separate "run a workflow" tool to choose between.

A workflow DEFINITION can also be written from the chat, by an agent that has
opted in (`behavior.workflow_authoring` — off by default; see [invariant
39](#design-invariants) for why this one is a switch when `todo_write` is not):
`get_workflow(name)` reads a definition and `save_workflow(...)` creates or
updates one, in a shape the model can hold — steps, their agents and their
edges by NAME, never by id (`{name, description, steps: [{name, agent,
prompt, gate, gate_pass, gate_fail, pause_before, compact_before, on_success,
on_failure}], budget}`, edges naming a step or `end`; the save tool's
description lists the agents on offer). Saving under a name that exists
replaces that definition — an update, not a second workflow — and a step that
keeps its name keeps its id, so a retry and an execution in flight still name
the same step; a nameless step reads back as `Step N`, which is what saving it
back then stores. Because names are the model's handles, the store holds every
definition to them — the hub editor's included: a step name denotes one step
(case-insensitively) and `end` is not one (`NormalizeWorkflow`), so what the
hub saves is always something the model can read back and edit. Every save is
APPROVED first: the tool is approval-gated on
its own (not through the agent's `approve_tools`), and the approval card in the
chat is the review — the definition, drawn as in the hub, and, when it replaces
one, the stored definition diffed line by line. A save that would not land
(an unknown agent, a duplicate step name, an edge to nowhere, no description)
never reaches the person: it is refused to the model at once, as text, and
nothing is written. A saved workflow can be started in the same turn —
`spawn_task(workflow=name)` reads the store, not its own listing — and, like
any edit, changes nothing already in flight. Neither tool exists on a
background run: a step cannot write definitions.

Each step carries a STABLE id, so inserting a step above another does not
renumber what a run in flight, a retry, or a record of what happened is naming.
A retry re-runs the step the execution stopped at: its turn is the retry
prompt (why, and to resume from the progress made) followed by the step's own
prompt again — a gate's verdict rule included — so nothing the step needs is
left to inference from the failed attempt.
An execution's `state` stores a SNAPSHOT of the definition: editing a workflow
never steers a sequence already in flight (the rule a task's inherited
configuration already follows).

A definition may carry a **budget** — `budget.max_steps`, `max_tokens`,
`max_minutes`, each zero for no bound, and `max_laps`, whose zero is the
default of 3 — that every execution of it answers to. Steps count launches
(retries included; at most `MaxStepRuns`), tokens the input plus output of
every model call on the execution's session, minutes the step runs' own time
(a pause on a person's approval costs nothing), laps the times one backward
edge is taken. Each is checked when the driver is about to launch the next
step and again before a retry, never mid-run: over any bound the execution
stops, failed with the reason (`budget exhausted: 4 of 3 tokens`), and a
retry is refused before it runs anything. The budget is snapshotted into the
state with the steps.

A start with no run asking — a person's `Run…`, a trigger's fire — leaves a
**note** on the conversation (an annotation, people-only: `display.kind`
`workflow_started`, with the task id, the workflow, the brief and who started
it). It is the exchange's question where the tool call would have been: the
result's wake-up run is labeled by it in the trace panel (`▶ build (you)`,
`▶ build (cron @daily)`) and jumps to it — so a trace card is always one
exchange, a question and what answered it, whether the question was a
message, a `spawn_task` call, or a note. The chip itself is the label alone —
what started, who asked — and opens the execution: the brief is read there,
in the task's detail with the steps and the transcript, not repeated in the
conversation.

Work can also start with no conversation asking, through a **trigger**:
`kind: cron` fires on a schedule (five fields, or `@hourly` / `@every 30m` —
no seconds field, and `@every` no shorter than a minute),
`kind: webhook` when something POSTs to `/hooks/:id`. What it starts is its
`target`: `workflow` (`workflow_id`) fires the same start `POST
/workflows/:id/runs` makes — an execution into the trigger's `session_id`,
reporting back there — and `agent` (`agent_config_id`) sends the brief as a
MESSAGE of that conversation, run by that agent under the conversation's own
sandbox binding: the scheduled question, its reply the next turn, with a
`trigger_fired` note before it so the reader knows an automation asked — the
note's chip is the label alone (which agent, which trigger), and the message
it precedes is the brief (a session busy with a run refuses, like a session
at its cap). Either way the
turn or execution is led by the `brief` its author wrote in advance, so the
rule that someone who knows writes the brief holds; a webhook's body (up to
64 KB) is appended to it as the payload. A webhook proves itself by signature, not
token: `X-Timestamp` (UNIX seconds, within five minutes of the server's clock)
and `X-Signature-256` = hex HMAC-SHA256(secret, `timestamp + "." + body`) —
the secret is minted at creation and shown in that response only (rotate it
for another). A delivery fires ONCE: the same timestamp and body sent again
inside the window — a sender's retry, a captured request — is a replay and
answers 409, so a sender that wants a second run sends a new timestamp (the
guard is in memory; a restart inside the five-minute window is the one gap).
Only a delivery that FIRED is held: one refused before anything started —
the session busy or at its cap, the server draining — may be resent as it
was, and fires then.
Cron ticks missed while the process was down are not replayed;
a tick that finds the session at its background-task cap, or busy with a run,
is refused like any start would be, and that refusal is what the trigger then
shows as its `last_error` (`last_started_id` is the task or run the last
fire started, empty when it started nothing). Deleting the session or the workflow deletes its triggers; a
deleted agent leaves its triggers standing, failing with the reason, to be
re-pointed.

| Method | Path                        | Description                                     |
|--------|-----------------------------|-------------------------------------------------|
| GET    | `/workflows`                | List definitions                                 |
| POST   | `/workflows`                | Create definition                                |
| GET    | `/workflows/:id`            | Get definition                                   |
| PUT    | `/workflows/:id`            | Update definition                                |
| DELETE | `/workflows/:id`            | Delete definition (executions keep their snapshot) |
| POST   | `/workflows/:id/runs`       | Start an execution for `session_id` with the brief `input`, optionally binding a still-unbound session first (`sandbox_id?`, `work_dir?` — the same first-run bind a run makes, so the steps have the composer's project) — 201 with the task; 400 no runnable steps / agent gone / an invalid binding, 404 unknown workflow or session, 409 the session's background-task cap or a bind that keeps losing to concurrent config edits (retry) |
| GET    | `/triggers`                 | List triggers (`?workflow_id=` for one workflow's); secrets never shown, only their tail |
| POST   | `/triggers`                 | Create — `{target, workflow_id | agent_config_id, session_id, kind, schedule?, brief, enabled}` (target inferred from the id when omitted); a webhook's `secret` is in this response only |
| GET    | `/triggers/:id`             | Get one                                          |
| PUT    | `/triggers/:id`             | Update (the kind cannot change; secret and fire record are kept) |
| DELETE | `/triggers/:id`             | Delete (off the clock at once)                   |
| POST   | `/triggers/:id/fire`        | Fire by hand — `{payload?}`; 201 with the task (workflow) or `{run_id}` (agent turn), 400 disabled, 409 the session's cap or a run in flight |
| POST   | `/triggers/:id/rotate-secret` | Mint a webhook trigger a new secret, returned once; the old one stops working |
| POST   | `/hooks/:id`                | The webhook itself (outside `/api/v1`, no token): signed with `X-Timestamp` + `X-Signature-256`, body = payload; 201 with the task or `{run_id}`, 401 bad or stale signature, 409 a replayed delivery |

In the UI, workflows are a place of their own — the sidebar's **Workflows**
button, beside New, opens the hub in the middle column: its Definitions (one
line per workflow, which opens on a click to its description and the sequence
drawn as a flowchart; the editor, `Run…` into a conversation of your choice,
each workflow's triggers), every Trigger — of either target — one line each,
opening to where it fires, its brief and how it last went, and the form to add
one, and every Run across conversations, live (a row
opens its conversation with the execution's detail in the Inspector). All
three lists page past 25 rows. They are not a settings tab: a workflow is
authored once and then WATCHED, and a trigger runs when nobody is looking.
From a conversation, `/workflow <name> <brief>` in the composer (typing `/`
offers the commands, walked with the arrow keys) starts one into it, the same
start `Run…` makes.

Executions are tasks: `GET /sessions/:id/tasks` lists them (`kind:
"workflow"`), `GET /tasks?kind=workflow` lists them across sessions, and
`/tasks/:id/stop`, `/retry`, `/dismiss` act on them — a stop
ends the whole sequence, not just the running step, and a retry resumes from
the step it stopped at, keeping the steps that already succeeded. Only a
FAILED execution retries — re-running a completed or cancelled one would repeat
its side effects — under the same attempt ceiling and per-session cap every
task answers to. A restart fails whatever was running, at the step it reached,
for the same reason. Deleting a session stops its tasks first — executions
included — so no step keeps causing side effects after the row is gone.

`GET /provider-types` (read-only) lists the registered backends as machine
facts — `type`, `auth_modes`, `unsupported` request features, and the global
key `setting_key` — straight from the server's provider registry, which is
also what validation and provider construction derive from. The UI's
capability hints read this endpoint, so they cannot drift from what the build
enforces; adding a backend is one registry entry plus a frontend metadata row.

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

Create and update validate the config STRICTLY and store it in canonical
form: a type mismatch on a known field (`persistent: "yes"`) or a missing
required field (docker `image`, ssh `addr`/`user`) is `400` — accepted, it
would bind sessions to a config that can never build, and once referenced the
identity freeze would block its own repair. Canonical means an ssh `addr`
always carries its port (`host` is stored as `host:22` — the backend dials 22
either way, and identity comparisons must not read the spelling difference as
a different machine), paths lose trailing slashes, and unknown keys are
dropped. One decoder answers every question about a config — save-time
validation, the content comparison, the identity freeze — so they cannot
disagree.

`DELETE` refuses (`409`) while any session is bound to the sandbox: the
binding is permanent, so removing its target would leave those sessions
failing every run with no way back — delete the sessions first. The refusal
is decided by the delete statement itself (a `NOT EXISTS` guard over
sessions), and the bind carries the mirror-image `EXISTS` guard, so a
first-run bind racing a delete cannot leave a session pointing at a vanished
config. (Conversely, deleting the last session bound to a
`(sandbox, work_dir)` pair releases its cached live instance — with holders
draining first: an in-flight run or an open terminal keeps the instance alive
until it finishes, and only then is the ssh connection or docker container
closed.)

`PUT` freezes a referenced sandbox's **identity fields** — `type`, ssh
`addr`/`user`/`work_dir` (the ssh user picks the account: its home and
permission view are a different file system even at one address), docker
`host_dir`/`persistent`/`container_name`: sessions bound the config id on the
promise that it keeps meaning the same file system, so an update that would
move it is `409` while references exist. Everything else stays freely
editable — `name`, credentials (key rotation is routine), `image`, `network`,
the docker exec `user`, `runtime` and limits change the execution
environment, not where the data lives.

A config carries two monotonic counters (both 1 at creation), each fencing a
different race. `revision` is the row's version: EVERY write bumps it, `PUT`
is a compare-and-set against the revision the client read (a concurrent
update means `409`, re-read and retry — no lost credential rotation, no
identity check against a stale row), and a first-run bind lands only against
the revision its workdir was validated on (a concurrent update makes it lose
and re-validate; a config that keeps moving exhausts the bind's three
attempts as `409`). `runtime_gen` is the CONTENT generation: it bumps only
when the type or config payload actually change, and it is what the
live-instance cache keys on and what retires instances and severs web
terminals when an update lands — so a rename never tears down a running
container or a live shell, while a credential rotation retires every
instance the moment it commits; a run that read the old config just before
can finish on it, but no later run or late-registering terminal ever shares
old credentials, an old image or an old mount. Integers, deliberately not a
version-history table: nothing keeps old generations runnable — updates
apply to everyone at their next run.

`terminal.open` validates a non-empty `work_dir` with the same rules a
binding does and uses the canonical form; a value the backend would silently
rewrite (a docker path outside `/workspace`) is refused instead of opening a
shell in a different directory than the client displays. An empty `work_dir`
stays valid — it means the sandbox's own default.

Every response carries a computed `terminal` boolean — whether the sandbox can
host an interactive web terminal (`ssh` always, `docker` only with
`persistent: true`, `local` never, by design). The chat top bar's terminal
button is enabled only when some sandbox advertises it; the session
itself runs over [`/ws/terminal`](#terminal-endpoint--get-wsterminal).

The same capability rule gates `exec_command`'s **persistent shells**: on
terminal-capable sandboxes the tool schema offers a `session_id`, and a named
shell is held open between calls so `cd`, exported variables and an activated
environment survive; on `local` and ephemeral `docker` the field is absent from
the schema rather than silently ignored. Named shells are scoped to one run —
its teardown closes them (an approval pause included, so a resumed run reopens
its sessions fresh). Tool output toward the model is capped above the SDK
defaults: file reads at 64 KiB (whole source files), exec output at 32 KiB per
stream (truncation keeps head and tail).

Responses also carry two computed workdir fields, which the composer's
pre-binding project picker prefills and gates on. `default_work_dir` is always
the **execution view** — the directory commands actually run in — so it never
disagrees with what the model reports as its working directory:

| Type                 | `default_work_dir`                 | `work_dir_editable` |
|----------------------|------------------------------------|---------------------|
| `ssh`                | `config.work_dir` (may be empty — a session binding then requires an explicit directory) | `true` |
| `local`              | the server `--workspace`, absolute | `true`              |
| `docker` persistent  | `/workspace` — the mount point     | `true` (constrained to `/workspace` or a subdirectory) |
| `docker` ephemeral   | `/workspace`                       | `false`             |

A persistent container's session may work in a **subdirectory of the mount**
(`/workspace/<project>`): the mount point never moves, but each project in the
mounted tree gets its own working directory — UI, binding and the model's own
`pwd` all show the same container-side path. Where the mounted data lives on
the **host** is a different concept: `config.host_dir`, the directory
bind-mounted at `/workspace` (empty = the server `--workspace`). It is
deliberately not called a working directory — it says where the files are, not
where commands run.

### Playground — `/api/v1/playground`

| Method | Path                   | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
|--------|------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| POST   | `/playground/generate` | One-off model call — `{agent_config_id, model?, system_instructions?, input_items, model_settings?, tools?, output_schema?, stream?}`; uses the agent's provider credentials, touches no session, records no run. `model_settings` overrides the agent's settings; `tools` are schema-only definitions (`{name, description?, parameters?}`) echoed from the traced request so the model can emit function calls — they are never executed (the Replay dialog also folds the traced handoffs in as such tools). `output_schema` (`{name?, schema, strict?}`, echoed from the generation span) replays structured output as structured output. `stream: true` switches the response to SSE: `delta` / `reasoning` text events as they arrive, then one `done` (`{output, usage, duration_ms, ttft_ms}`) or `error` (`{message}`); aborting the request cancels the model call. Backs the trace panel's "Replay" dialog. |

### ChatGPT OAuth

Login, logout, and status are per-agent, under the agent resource — see
[Agents](#agents--apiv1agents). The browser OAuth redirect lands on a temporary
listener at the fixed ChatGPT port (localhost:1455), not on an API route.

A `chatgpt_login` agent talks to the Codex backend
(`chatgpt.com/backend-api/codex`), which differs from the standard API in two
ways the bridge absorbs: request bodies are rewritten (`store: false`, no
`previous_response_id`, input sanitized to the fields the backend accepts),
and **only streaming requests are accepted** — the provider is wrapped in
`agents.NewStreamOnlyProvider`, so blocking callers (title generation,
compaction summaries, playground) are served by an internal stream instead of
a non-streaming POST the backend would 400.

### Secret handling

Secret fields are **write-only**. GET responses return them masked as `********`;
the plaintext is never sent to a client. On write:

- sending the `********` mask back keeps the currently stored value,
- sending a new value replaces it,
- sending `""` clears it.

This lets the UI round-trip whole objects without ever seeing the plaintext.
Masked fields: provider `api_key`, each agent `fallback_models[].api_key`, MCP
`headers` values and `oauth_client_secret` (`streamable_http` only), SSH sandbox
`password`, and the `brave_api_key` setting. A model-API key crosses exactly one
surface — the provider — which is what giving providers their own entity bought.

**A masked key round-trips only to the destination it was stored for.**
Changing a provider's `type` OR `base_url` while keeping the `********` mask is
rejected with 400 (replace the key or clear it) — restoring it would send the
previous backend's real credential to another endpoint. Fallback entries restore their masked keys strictly by
`(provider_type, base_url, model)`, never across providers or endpoints and
never by position; an unmatched mask clears.

**At rest, secrets are sealed under one process key.** Set `AGENTS_SECRET_KEY`
(or `--secret-key-file`) to a 32-byte key — `openssl rand -base64 32` — and
every credential column is stored AES-256-GCM encrypted
(`enc:v2:<key id>:…`, the key id being the first bytes of the key's SHA-256):
provider `api_key` and ChatGPT token, MCP `oauth_token`, `oauth_client_secret`
and `headers`, SSH sandbox `password`, webhook `secret`, fallback `api_key`s,
and the settings the registry marks secret. Possession of the database is then
not possession of every upstream credential. Each value is bound to its place
— `table.column`, plus the field for a credential inside a JSON column — as
the cipher's additional data, so a ciphertext moved to another column (a
provider's key planted as another provider's, or as an MCP header bound for
someone's endpoint) does not open there; and a value pasted in through the
API that already looks sealed is sealed again as the text it is, never stored
as someone else's ciphertext. Without a key the server logs one warning and
stores plaintext — the single-user workbench. Rows written before a key was
set stay plaintext until their next write, and open either way; a sealed row
with no key, or under another key, is a loud error naming the key ids, never
ciphertext handed out as a credential. The first start with a key seals a
canary (`settings.secret_key_check`) and every start after opens it, so a
key that is missing or not the one refuses to start with one message —
rather than the first Settings panel failing to load. There is no
rotation: losing the key loses the secrets; the recovery is the key itself
or a fresh database.

### Health

| Method | Path      | Description                                                    |
|--------|-----------|----------------------------------------------------------------|
| GET    | `/health` | Liveness probe (unauthenticated) — returns `{status, version}` |

### OpenAPI

A generated OpenAPI 3.1 document (YAML) is served at `GET /api/v1/openapi.yaml`
(unauthenticated). It is generated from swag annotations on the handlers via
`make openapi` in `cmd/agents-server`, and the frontend's request/response
types are generated from it in turn (`npm run gen:api` in
`internal/web/frontend`, writing `src/lib/apiTypes.gen.ts`). CI fails when
either generated file is stale, and runs `npm run lint` (ESLint: rules of
hooks, exhaustive deps) on the frontend — so a handler annotation change is
three commands: `make openapi`, `npm run gen:api`, commit both outputs. There
is intentionally no bundled Swagger/Redoc UI — import the YAML into your own
tool.

## WebSocket protocol

Endpoint: `GET /ws`

> The target shape this protocol is moving toward — one `run.entry` event,
> entry ids on streaming deltas, SDK-owned error codes — is frozen in
> [PROTOCOL.md](PROTOCOL.md). What follows is what ships today.

The WebSocket does not accept a token in the query string. After connecting, the
client must authenticate at the application level by sending
`{"type":"auth","token":"..."}` as the first message. The server replies with
`{"type":"auth.ok"}`.

An inbound frame over 1 MiB closes the socket with `1009`, and no
`run.error` can follow a close frame — so the composer refuses a prompt past
that size before sending it.

After `auth.ok` the server pings every 25 seconds and drops a connection that
answers no ping for 60 — a half-open connection (NAT idled out, client gone
without a close frame) would otherwise pin its goroutine and buffers until TCP
keepalive notices, hours later. Browsers and standard WebSocket libraries
answer pings automatically; a custom client only needs to keep reading. A
pong counts only when the server is reading, so a handler about to not read
for a while — the terminal endpoint dialing a host or pulling an image —
lifts the deadline for that stretch and re-arms it after.

All messages use the envelope format `{"type":"...", "payload":{...}}`.

Runs live in the runner's hub, independent of the connection, and their events
are a **broadcast bus**: every authenticated connection is attached to every
run's stream — on connect (all in-flight runs, with a replay of their buffered
events) and automatically when any run starts or resumes, no matter which
connection (or REST call) started it. Two browsers on the same session both
watch the conversation live. A dropped socket does not cancel a run; after
reconnecting the server re-attaches the connection, and `run.subscribe` remains
available to resume from a specific cursor (`from_seq`) without a full replay.
The replay ring holds a run's last 512 events; its `run.started` is pinned
outside the ring, so a subscriber from seq 0 is told which run this is even
when the ring has long moved past the start (a browser reloaded a minute into
a run streams live again instead of showing the session idle until it ends).

### Client → Server

| type            | Description                                                                                                     |
|-----------------|-----------------------------------------------------------------------------------------------------------------|
| `run.create`    | Start a run — `{session_id, input, agent_config_id?, sandbox_id?, work_dir?}` (sandbox/workdir matter only until the session's first sandbox-carrying run binds them) |
| `run.subscribe` | (Re)attach to a run's event stream — `{run_id, from_seq?}` (omit `from_seq` or `0` replays everything retained) |
| `run.cancel`    | Cancel an in-flight run — `{run_id, mode?}`; `mode: "graceful"` finishes the current turn, default aborts       |
| `run.inject`    | Inject input into the live run — `{run_id, queue, input}`; `queue: "steer"` changes course inside the current exchange, `"next_turn"` is consumed at the next turn boundary, `"follow_up"` starts a new exchange once this one finishes |
| `tool.approve`  | Approve a pending tool call — `{tool_call_id}`                                                                  |
| `tool.reject`   | Reject a tool call — `{tool_call_id, reason?}`                                                                  |

### Server → Client

| type                    | Description                                                                                                                                             |
|-------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `run.started`           | Run begun — `{run_id, session_id, input}`; `input` is the user prompt, so a browser that didn't send it can render the user bubble. A background task run additionally carries `{task_id, parent_session_id, parent_run_id, tool_call_id, label}` — clients key task state by the durable `task_id`, route events by `run_id`, and send it to the parent session's task list, never a chat timeline |
| `run.agent_start`       | Agent taking its turn — `{run_id, agent_name}`                                                                                                          |
| `run.step`              | Streaming text delta — `{run_id, delta}`                                                                                                                |
| `run.reasoning`         | Streaming reasoning delta — `{run_id, delta}`                                                                                                           |
| `run.message`           | One completed assistant message: a turn's full text, interim narration or final answer, authoritative over its `run.step` deltas — `{run_id, text}`     |
| `run.reasoning_item`    | One completed reasoning block: a turn's full thinking text, authoritative over its `run.reasoning` deltas — `{run_id, text}`                            |
| `run.tool_call`         | Tool invoked — `{run_id, tool_call_id, tool_name, arguments, needs_approval}`                                                                           |
| `run.tool_progress`     | Partial output from a running tool — `{run_id, call_id, tool_name, delta, renderer?}`; `delta` appends to what the client holds for the call, `renderer` is a display hint (e.g. `terminal`) |
| `run.tool_result`       | Tool output — `{run_id, tool_call_id, output, title?, summary?, renderer?, is_error?, extra?}`; the optional display fields mirror the stored output entry's `display` (`extra` is the tool's `Details` bag), so the live card carries the same data a reload rebuilds. A multimodal result's `output` is the Responses content list as JSON (`[{"type":"input_text",…},{"type":"input_image","image_url":…},{"type":"input_file",…}]`, SDK spec §2.7b) — the card shows the image and offers the file; anything else is text |
| `run.handoff`           | Agent handoff — `{run_id, from, to}`                                                                                                                    |
| `run.compaction`        | Session compaction running at end of turn — `{run_id, phase: started\|finished, detail?}`                                                               |
| `run.output`            | Final output — `{run_id, final_output}`                                                                                                                 |
| `run.interrupted`       | Paused for tool approval — `{run_id}`; NOT final: the decision resumes the SAME run id, and its events continue the sequence on the same subscription. Sent only once the pause is durable (the `pending_approvals` row written) — a pause that cannot be recorded ends the run as `run.error` (`persist_error`) instead, so nothing is ever announced as awaiting a decision nobody can make |
| `run.diagnostic`        | Trouble the run survived — `{run_id, type, code?, message?, details?}`; `type` is an open vocabulary (`model_retry`, `model_fallback`, `tool_panic`, …), so show unknown kinds generically |
| `run.gap`               | This connection fell behind and events were dropped — `{run_id, dropped, last_good, next}`; resubscribe from `last_good` to refetch. A gap with `last_good: 0` is the ring having moved past the run's start before this connection attached: nothing to refetch (the UI does not ask) |
| `run.error`             | Error — `{run_id?, session_id?, code, message, guardrail?, stage?}`; `session_id` is set when the failure precedes `run.started` (e.g. `session_busy`, `session_not_found`); `guardrail`/`stage` are set when `code` is `guardrail_tripwire` |
| `run.cancelled`         | Cancelled — `{run_id}`                                                                                                                                  |
| `session.title_updated` | Title changed — `{session_id, title}`                                                                                                                   |
| `task.updated`          | A background task moved — the task row (`task_id`, `status`, `kind`, `state`, `attempt`, `dismissed`, a paused one's `pending_call_id`…) as the store has it; on the task's run stream when the hub holds that run, else broadcast to every connection |
| `session.sandbox_bound` | The session's first sandbox-carrying run permanently bound `(sandbox_id, work_dir)` — `{session_id, sandbox_id, work_dir?}`; published exactly once, by the run that won the bind |
| `trace.span`            | Trace span — `{run_id, trace_id, span_id, error?, data?, payload_omitted?, ...}`; `payload_omitted` says the 256KB live cap replaced the payload fields, which the stored row still has |

Generation spans carry the full model request/response in their `data`
(`model`, `system_instructions`, `input`, `tools`, `model_settings`,
`handoffs`, `output_schema`, `output`) — the trace panel renders these when
you expand a generation span, so you can see exactly what each call sent
after compaction/filters, including MCP/skill tool definitions. Those payload
fields are nearly all of a session's trace bytes (every generation span
carries the whole conversation as its input — a hundred spans of a long
session run to tens of MB), so the panel opens with the SUMMARY listing
(`?summary=true`: rows without them, marked `payload_omitted`) and fetches
one span whole (`GET /sessions/:id/traces/:span_id`) when it is opened —
what a session's history costs to open no longer grows with what its model
calls carried. Payloads past `trace_span_data_kb` are replaced with a
truncation marker in the row itself; set
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
├── cmd/root.go                 CLI flags, start-up and shutdown ordering
├── cmd/wire.go                 the composition root: stores → bridge → handlers → auth → server
├── internal/
│   ├── server/                 Gin engine, routing, WS upgrade + heartbeat
│   │   ├── auth.go             bearer middleware (AuthFunc), the auth-exempt list
│   │   ├── ratelimit.go        per-IP budgets; AuthGuard (failed-credential budget)
│   │   ├── audit.go            the audit middleware (successful mutating requests)
│   │   └── server.go           engine setup, body cap, CSP, static SPA
│   ├── authn/                  who is calling: token mode, OAuth (PKCE) login, PATs
│   ├── secrets/                AES-256-GCM box that seals stored credentials
│   ├── handler/                HTTP handlers (one file per resource)
│   │   ├── authz.go            the two authorization rules as route gates
│   │   └── conn_registry.go    per-owner WebSocket broadcast bus
│   ├── bridge/                 the runner and what it orchestrates
│   │   ├── agent.go            assemble a full agent from DB config
│   │   ├── runner.go           stream execution, resume after approval
│   │   ├── stream_bridge.go    SDK stream events → protocol envelopes
│   │   ├── run_hub.go          per-run event hub (buffering, seq resume, status)
│   │   ├── approvals.go        HITL approval persistence & resolution
│   │   ├── retention.go        the maintenance loops: approval reaper, trace/audit/token/wake-up pruning
│   │   └── ...                 tasks, workflows, triggers, tracing, provider resolve
│   ├── mcpservers/             live MCP connections behind stored configs; the MCP OAuth flow
│   ├── providers/              the registry of model-provider backends; the ChatGPT login
│   ├── sandboxes/              live sandbox instances behind stored configs; exec_command trust
│   ├── guardrails/             stored + built-in guardrail definitions → SDK guardrails
│   ├── settings/               the settings registry and the typed reader (incl. the proxy client)
│   ├── docs/                   generated OpenAPI 3.1 document, swagger.yaml (make openapi)
│   ├── store/                  data layer (bun ORM; SQLite or PostgreSQL, 22 tables — see Database)
│   ├── protocol/               wire types — WS messages, REST error envelope, the audit record
│   └── web/                    embedded SPA static files
└── {workspace}/skills/         agent skills managed via API (runtime dir, not in the repo)
```

### Request flow

1. A client starts a run — `run.create` over WebSocket or `POST /sessions/:id/runs`
   over REST. Both call `runner.StartRun`, which registers the run in the shared
   run hub and executes it in the background, independent of the caller's
   connection.
2. The runner loads config from the database and calls `BuildFullAgent` to
   assemble the agent with its provider, MCP tools, sandbox, guardrails,
   memories, and hooks, then calls the SDK's `agents.Run()` to execute.
3. Streaming events are published to the hub, which fans them out to every
   subscriber (WebSocket connections and SSE streams) and buffers them for replay
   so a reconnecting client can resume from a sequence number.
4. If a tool requires approval, the run pauses and the pending approval is
   persisted; it resumes on `approve`/`reject` (over either transport) and
   survives a server restart.
5. History is persisted per turn as the run progresses — a cancelled or failed
   run keeps every completed turn — and the session title is generated in
   parallel with the first run rather than after it.

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
   **A mask never round-trips across a destination change**: restoring a
   stored key under a changed `provider_type` OR `base_url` sends one
   backend's real credential to another provider or endpoint, so the update
   is rejected (agents) and fallback entries restore strictly by
   `(normalized provider_type, normalized base_url, model)` — never by
   position; an unmatched mask clears.
   **The mask resolves inside the store's transaction.** The store's `Update`
   (providers, agents, MCP servers) and `SettingStore.Modify` read the stored
   row — `FOR UPDATE` on PostgreSQL — and hand it to the handler's callback
   before writing; a handler must never `Get` first and resolve outside, or
   two concurrent edits let a client echoing the mask restore the key the
   other just replaced. A rule the callback refuses returns
   `badRequestError`, which `saveError` maps.
10. **OAuth-class tokens never leave the server.** Store them in their own
    column with `json:"-"`, exclude the column from CRUD updates
    (`ExcludeColumn`), and expose only a derived boolean
    (`has_oauth_token`, `chatgpt_logged_in`). Do not reuse a masked token
    string as a truthiness signal.
11. **An OAuth grant persists as a self-contained refreshable unit, through one
    writer.** The stored payload carries the token AND its refresh context —
    token endpoint plus (possibly dynamically registered) client credentials —
    and every mutation flows through `mcpservers.persistGrant`: the initial
    authorize and every later refresh, including a rotated refresh token
    (`persistingTokenSource`). Restored and live connections must use the same
    refreshing `oauth2.TokenSource` machinery — never a static snapshot of the
    access token, which is exactly the two-mechanism drift (in-process refresh
    worked, restart silently didn't) this replaced.

**Store layer**

12. **No bun `default:` tags on booleans.** bun swaps a zero-value field for
    SQL `DEFAULT` on insert, so `default:true` silently enables a row created
    with `enabled=false`. Use `notnull` and set the value in Go.
13. **Deleting a referenced resource fails loud at use, never silently skips a
    safety feature.** Guardrail names that no longer resolve fail the agent
    build (a guardrail that appears enabled but never runs is a security hole);
    dangling MCP/skill ids are filtered with a visible count in the UI. Pick
    one of those two behaviors deliberately for any new reference.

**Chat / run streaming**

14. **Run events are a broadcast bus, not a reply channel.** Every
    authenticated WS connection is attached to every run's stream — on
    connect (all live runs, with replay) and through `Runner.OnRunAttach`
    when a run starts or resumes, whether it was created over WS or REST.
    Two browsers on the same session both watch the conversation live;
    `run.started` carries the prompt (`input`) so a browser that didn't send
    it can render the user bubble. Never wire an event to "the connection
    that asked" — that is exactly the bug this replaced. The bus is only as
    good as a late joiner's ability to PLACE what it hears: `run.started` is
    the sole event that maps a run id to its session, and the client drops
    every event of a run it never saw start, so the hub pins each run's latest
    `run.started` outside the 512-event replay ring and hands it to a
    subscriber whose cursor lies before it, first, whenever the ring itself
    no longer will (`RunHub.SubscribeSeq`) — a reload a minute into a run
    used to show the session idle until it ended. The client, in turn, does
    not re-subscribe on a `run.gap` whose range the ring has already evicted
    (`last_good: 0`, or the cursor it already asked for): that would replay
    the whole ring every few seconds for the run's life.
15. **Protocol constants have one definition per side.** Event types
    (`run.error`, …) and error codes (`session_busy`, …) live in
    `internal/protocol` (Go) and `src/lib/protocol.ts` (TS mirror). Emitters
    and consumers reference the constants, never string literals — a typo must
    be a compile error, not an event that silently never fires. Adding an
    event means updating both files. The mirroring obligation stops at the WS
    contract: the REST error envelope (`internal/protocol/apierror.go`) is
    Go-only, because it has two Go emitters that cannot import each other
    (`handler` and `server`) but only one browser consumer, and that consumer
    reads `error.message` alone — it never branches on the code.
16. **A streamed turn must equal its reload.** The streaming path
    (`src/lib/streamReducer.ts` pure transforms, applied by `useAgentSocket`)
    and the replay path (`buildTimeline` over persisted ENTRIES) must produce
    the same `turn.parts`; `src/lib/timeline.test.ts` pins this isomorphism — run
    it via `npm test`. Intentional differences are documented and asserted
    there (currently: handoff parts are live-only; a rejected call's status
    replays as completed). A new part type or field lands on BOTH paths plus
    the shared types in `timeline.ts`, or the test fails.
17. **Terminal run events reconcile against the store.** Every terminal event
    handler (output/error/cancelled) applies its optimistic parts and then
    reloads the persisted timeline as the authority. Exceptions must be
    deliberate and listed here — currently only `guardrail_tripwire`, which
    skips the reload to keep the retracted-answer view the SDK never persists.
18. **The streaming block patches the DOM; user intent beats the pin.** The
    live text is morphdom-patched (`StreamingMarkdown.tsx`), never rewritten
    via innerHTML — node identity is what keeps a text selection alive across
    deltas, so anything that replaces those nodes wholesale is a regression.
    Bottom-following (`useScrollToBottom`) re-fires on every content growth
    (the dep includes streamed text length) and yields to explicit user
    intent: an upward wheel/drag or an actively changing selection unsticks
    immediately; a stale leftover selection must NOT block re-sticking when
    the user scrolls back down (recency windows, not standing state, arbitrate
    the races with the pin's own trailing scroll events).
19. **A branch move obsoletes every client view of the old path.** Regenerate
    and attempt-switch are server-side appends (`POST /sessions/:id/branch`);
    the client reconciles by refetch, and four rules keep the abandoned
    attempt from lingering on screen: the timeline's `on_path === false`
    filter applies even when no fork exists yet (right after the switch the
    old answer is the user entry's ONLY child — the new attempt has persisted
    nothing); a branch move bumps the session's timeline generation so a
    fetch launched before it is dropped on resolve, never applied; the
    live-tail merge re-appends only the CURRENT live run's turn
    (`mergeLiveTail(…, liveRunId)`); and a pending approval whose run has
    off-path entries stays out of the timeline — the row itself is kept, so
    switching back to the paused attempt re-admits its card and the pause can
    still resume.

**Background tasks**

20. **A task is a durable entity; a run is one execution of it.** `spawn_task`
    mints separate ids: the task row carries `run_id` (its current attempt),
    and `run.started` / `RunInfo.task` carry `task_id` — clients route events
    by run id and key task state by task id — `task_retry` mints a new run id
    on the same task row, and `run.started` carries the `attempt` so a client
    whose card shows a finished task can tell a new attempt from a replay. The transcript lives in a hidden child session. The
    spawn target is an agent config by name; an empty `agent_name` (or the
    `default` / `self` / `current` aliases) runs the task with the spawning
    run's own agent — a config actually named that way wins. Task events use
    the same broadcast bus, replay cursors, approval persistence, and
    retention as chat runs — a task-specific transport is how the two
    lifecycles would drift.
21. **The spawn card's durable truth is an appended update entry.** The hub's
    RunInfo is GC'd minutes after a run ends; when a task changes state the
    server APPENDS an update entry addressed to the spawn call's id — the
    label and summary on display's first-class Title/Summary,
    `task_id`/`task_status` in Extra as renderer state — and the read folds it
    into that call's display. The fold merges non-empty fields only, so a
    retry's working update cannot blank the failed attempt's summary — a
    summary-carrying update therefore also records `task_summary_attempt`
    (Extra merges per key), and the timeline drops a folded summary older than
    the card's `task_attempt` rather than show a voided failure as the current
    attempt's result. Appending is what removed the retry loop: a fast
    task can finish before the turn that spawned it is persisted, and the old
    rewrite hunted for a row that did not exist yet. An update may be stored
    BEFORE its target; folding associates them by call id afterwards. A
    non-terminal update is dropped when the task row is already terminal, so a
    reordered notify cannot roll a finished card back. The live path mirrors
    the fold: pre-terminal status rides run events (the card's badge), and a
    terminal outcome lands on the spawn card live via `applyTaskTerminal` —
    same shape as the replayed fold, terminal only, never backwards, and
    re-applied by `runToolCall` when the task's independent subscription
    outran the card. Durable status always comes from the folded entry, never
    from the hub after the fact. Completion wakes
    the parent at its next run boundary via a `[task-notification] ` input;
    the debt is a `wakeups` row inserted in the same transaction as the
    terminal status (invariant 32), consumed by an in-turn `task_status`
    read or delivered by the wake-up run — the auto-wake survives restarts
    via the startup sweep. The notification is ordinary user-role input identified by its text
    prefix. It never renders in the timeline — the chat top bar's Tasks button
    and the Inspector are the human-facing surfaces; the model reads the text
    verbatim. The prefix carries no privileged behavior: a user typing it
    merely hides their own message from the transcript view.
22. **The right side panel is a single-instance Inspector.** Traces, context
    usage, the task list, and one task's detail (live transcript + trace,
    assembled with the same streamReducer/timeline code as the chat) are lenses
    of one panel — a new inspection surface is a new lens, not a second drawer.
    Task detail accumulates live child-run events only while open
    (watchTask/unwatchTask). Persisted spans load on the first open of a lens
    that reads them (trace or context), not on session open — a stored
    generation span can carry a whole model request, and every session open
    paying that download for a panel nobody may open is the wrong default.
    Live runs stream their spans over the WS regardless. Run LINEAGE — which
    run's spawn a wake-up belongs to, what nests the "task result" card under
    its originating card — is recorded on the trace itself
    (`trace_events.parent_run_id`, stamped at launch from
    `LaunchRequest.ParentRunID`) and read directly. It is never derived from
    task rows, notification text or the rendered timeline: each derivation
    broke on a surface that does not carry its inputs (a fork copies traces
    but not task rows; a fold moves the notification out of the timeline).
23. **A task's terminal state is written exactly once, via row CAS.** The
    durable row is the terminal authority: `Finalize` (status + full result +
    notification debt in one UPDATE) only wins while the row is non-terminal,
    stop/approve claims race through the same CAS (`Finalize` vs
    `ReclaimWorking` — exactly one wins, and `hub.resume` refuses finished
    records as the second line), and `task_status` treats only the row as
    final — a hub-terminal run whose row hasn't landed is still `working`.
    That second line is one-directional: it rejects a resume that arrives after
    a stop landed, but `taskStopper.Stop` reads the hub status and publishes
    `run.cancelled` after, so the reverse interleaving — a stop that saw
    `RunInterrupted` just before an approval resumed the run — can still put a
    terminal event beside a live segment. The compensation is the post-resume
    task re-check in `ResolveApproval`, which cancels the run it just started
    when the row reads terminal. A new publisher of terminal run events must not
    rely on the guard: give the hub an atomic status transition instead of
    adding a third compensation. A graceful stop marks the hub record before
    signalling, so its clean finish lands as `cancelled` ("stopped after the
    current turn"), never as a completion. A hard cancel is equally
    unambiguous: once the run's context is cancelled, every stage that notices
    it — including a session lookup that never got to answer — reports
    `run.cancelled`, never a `config_error` or `session_not_found` the user did
    not cause, and the task's status follows that event. Cancellations consume
    their own wake-up debt (the user did it; completed / failed are the states
    worth waking the parent for). Deleting a session stops its run tree first
    (cancel + bounded wait on the done gate) so no write can land after the
    cascade — which then removes the whole tree, every hidden session at any
    depth, walking LIVE edges only (a stale row's child id may since belong to
    an unrelated session).
24. **One entry in, the same entry out.** The `entries` table stores whole
    `agents.SessionEntry` JSON, with only the columns the queries need lifted
    out. The server does not re-derive a display, a role, or provenance at read
    time — the runner already decided all three, and a reader that recomputes
    them can only produce a worse version that drifts. The messages table this
    replaced had a column per field the UI wanted, so `Source`, `Usage`,
    `Diagnostics`, `NestedUsage` and the parent link had nowhere to go and were
    dropped on the way in. Compaction soft-deletes (`compacted = true`) so the
    UI can still show what was folded, and appends a compaction CHECKPOINT
    whose payload names what it folded (`compaction.excluded_ids`) and carries
    the summary that stands in for it — which is why the model sees
    `[summary, kept…]`: the projection drops what the checkpoint excluded and
    renders its summary up front, reading the kept turns from the session
    itself rather than from a copy. The pass is branch-scoped: it sizes and
    folds only the ACTIVE branch — what the projector sends — so an abandoned
    attempt neither trips the threshold nor leaks into the summary, and is
    never itself folded (it is already out of the model's view). A pass can
    also be forced by hand (`POST /sessions/:id/compact`, the Context panel's
    "Compact now"): Force skips only the threshold — every other guard holds,
    so a manual pass can never fold what an automatic one would have kept.
    The summarization request carries the folded prefix as ONE plain-text
    transcript under a single user message: the conversation is the summary's
    DATA, not the summary model's history, so no provider's history validation
    (call/output pairing, DeepSeek's reasoning round-trip) can reject the pass.
    The TRANSCRIPT is decoupled from the fold: folded entries render in place,
    in full — history is not what compaction deletes — and the checkpoint
    renders where it sits as an inline "~39k → ~7.8k tokens" marker with the
    summary one expand away. Which entries the model still reads is the
    Context panel's question, never the transcript's: hiding folded turns
    inside the marker made the conversation unreadable past every pass and
    broke everything that reads the rendered timeline (the trace panel's run
    grouping above all).
25. **Schema changes ship without migrations.** `CREATE TABLE / INDEX IF NOT
    EXISTS` is the whole story; a structural change to an existing table means
    dropping and recreating the database (dev-tool stance, decided
    deliberately). Never add ALTER TABLE migration machinery here. The
    no-migration stance is honest only if the mismatch is loud: startup
    probes every model with a zero-row SELECT (bun names every mapped
    column), so an old database file fails fast with a "delete and recreate"
    message instead of surfacing per-request as `no such column` — the
    models themselves are the schema version, no constant to forget to bump.
26. **Where a session stands is stored, not folded.** An append must not cost a
    read of the session: the SDK persists once per TURN, so a run appends many
    times, and folding the branch tip out of the entries each time made one run
    cost O(entries²) — over a log that only grows, since compaction
    soft-deletes. The tip and the highest sequence number live in
    `append_points`, keyed by `(session_id, gen)` like every other address of an
    entry row. It is not a cache: `appendTo`, `Clear`, `pop`, `ForkSession` and
    the compaction adapter's fold each write it inside the transaction that
    carried their change, so the two records cannot come apart, and
    `foldAppendPointIn` stays the definition they must agree with — field for
    field, not "close enough". `TestAppendPointMatchesTheFold` holds the paths a
    session walks in place, `TestForkCutOnAFoldedEntry` the copy a fork makes
    (a fork carries compacted rows too, so a cut landing on one must not make it
    the destination's tip), and
    `TestPersistCompactionParentsTheCheckpointAtTheSurvivingTip` a fold that
    takes the tip with it. A missing row falls back to the fold rather than
    reading as an empty session — that is what a database written before this
    table holds, and calling it empty would make the next append a new root and
    leave the whole conversation on an abandoned branch. `GetEntries` still
    folds the whole session on every call: a known cost, deliberately left,
    because a person pays it once per page while a run paid it once per turn.
27. **A session's `(sandbox_id, work_dir)` binding is immutable and
    server-authoritative.** The first sandbox-carrying run binds it
    (`BindSandboxIfEmpty`) and nothing changes it afterwards: there is no
    unbind, no rebind, and no PATCH — switching projects means starting (or
    forking into) another session, which is the composer Project picker's
    flow. From then on `startRunWithID` overrides whatever the client sends
    with the bound values; the top bar shows the binding as a read-only badge.
    A new binding is validated first and written only after the run is
    accepted: `planSandboxBinding` resolves and checks the pair (the config
    must exist; `resolveBindingWorkDir` canonicalizes the workdir per backend
    — ssh requires a fixed directory, docker persistent a `/workspace`
    subtree, ephemeral empty, local an absolute path; violations are 400 and
    bind nothing), then `hub.register` claims the session slot, and only then
    does the CAS write land — a run refused as busy/deleting/draining has NOT
    silently fixed the session's file system context (`hub.unregister`
    withdraws a registration whose bind failed). Runs with no sandbox never
    bind, so a chat-only session can still pick one later. The binding's
    target is protected in both directions, atomically: deleting a sandbox
    still referenced by sessions is refused with 409 by the delete statement's
    own `NOT EXISTS` guard, the bind carries the mirror `EXISTS` guard
    (`BindSandboxIfEmpty`), and an update that would change a referenced
    sandbox's IDENTITY — type, ssh addr/user/work_dir, docker
    host_dir/persistent/container_name, the fields that decide where the
    data lives — is refused the same way (`UpdateIdentityIfUnreferenced`),
    while credentials, name and limits stay editable. A bound session whose
    sandbox cannot be resolved or built fails the run loudly rather than
    degrading to a chat with no tools. Sandbox instances are cached per
    `(config id, runtime generation, workdir)` with a REFERENCE COUNT
    (`SandboxManager.Acquire`):
    runs and terminals hold their instance for exactly their lifetime, and an
    eviction (config update/delete, or the last bound session going away —
    `ReleaseSessionBinding`) closes an idle instance immediately but only
    DOOMS a held one, which the last release closes — an in-flight run or an
    open terminal is never torn off its connection. Task child sessions
    inherit the parent's pair through `Inherit` and bind their own hidden
    sessions with it.

28. **Every figure in the Context panel says which ruler it is on, and they are
    never mixed.** `/sessions/:id/context` reports three kinds of number and the
    panel draws them apart. (a) The WINDOW figures — `input_tokens`,
    `cached_tokens`, `cache_write_tokens` — are the provider's own counts for
    the last model call, covering everything it sent: history, system prompt,
    tool schemas. (b) `compaction_tokens` is *exactly what the pass compares* —
    `store.ActiveContextTokens` over the same rows, both sides resolving the
    active branch with the same walk (`activeBranchOfRows`): the most recent
    usage-bearing entry's TOTAL prices the history through itself, plus a
    character estimate for the turns since. A fold NEWER than that pricing
    invalidates it — the priced history included what the fold removed — so the
    figure is fully estimated (kept tail + summary + turns since) until the
    next call re-anchors it; without this the number would hold its pre-fold
    height, and a manual Compact would look like it did nothing. It is therefore mostly a provider
    number, not a character sum — a character sum would draw a threshold line
    that does not match the one that fires. The panel draws ONE bar (the
    window's), with the compaction threshold as a TICK on it and the exact
    comparison (`compaction_tokens / threshold`) as its own indented numbers
    row: the tick marks roughly where the fold lands on the window's scale,
    while the row keeps the comparison on its own ruler — the bar and the
    numbers never pretend to share one. (c) `conversation_tokens` and the
    `prompt` breakdown are character estimates (`CharEstimator`, ~4 chars per
    token): good for shares and for "roughly what does this cost", never for
    arithmetic against (a) or (b) — which is why the "In the window" section's
    percentages are shares of their OWN estimated total, one ruler throughout
    (only the section's header line relates that total to the declared window,
    which is configuration, not a measurement). The UI does not BADGE
    estimates — a badge is skipped by the eye that reads the digits, and four
    exact-looking digits claim a measurement the estimator never made. It
    renders them to the precision they have: two significant figures behind a
    `~`. A figure without one is the provider's own count, which is the whole
    labelling scheme. Sub-agents appear in none of them — a task runs on its own session with
    its own window (invariant 22's Inspector shows that one); what lands in the
    parent's context is the result text, counted like any other tool output.

29. **A workflow execution is a TASK, and it advances from the run's
    TEARDOWN, never from the callback of the run that started it.** An
    execution is a task of kind `workflow`; the SDK's task manager owns its
    lifecycle, and this server is the driver it calls back into (`Continue`:
    which step a finished run leads to; the launcher: how a step's run starts).
    A step is an ordinary run; when it ends, `postRun` — which every segment
    reaches, fresh or resumed — hands the outcome to the manager, which asks
    the driver and only then starts the next step. Hanging the advance off the
    starting call's own callback loses the sequence at the first approval: a
    paused step's run ends, the approval endpoint resumes it with NO callback,
    and that resumed ending is the one that may move the sequence on. The
    advance is the SDK's `Store.Advance` — a compare-and-set on
    `(status = working, run_id = the run that finished)` that lands the new
    state in the same write — so a superseded attempt's late callback cannot
    drive it, a stop that got there first wins, and an INTERRUPTED outcome
    advances nothing — it is a pause, not an ending. Because it is a task,
    stop, retry, the restart sweep, the approval expiry, the session-delete
    teardown, the cap and the wake-up debt are the task's, written once.

30. **A workflow runs OFF the conversation that asked for it, and starts only
    with a BRIEF written by someone who read that conversation.** The steps
    execute on a hidden child session, so a sequence's plan-then-write-then-
    check never enters the chat and no later turn pays for the whole
    procedure; what comes back is the result, through a wake-up. The steps
    still share that one session with each other, so a later step reads what
    an earlier one did — the isolation is between the sequence and the chat,
    not between the steps. It shares the parent's SANDBOX, which is what lets
    the deliverable be files rather than a description of files. The child
    session cannot see the conversation, so someone has to write its brief:
    the agent, through `spawn_task(workflow=name, input)` — a workflow's
    `description`, what the agent matches a request against, is required for
    that — the person, through `POST /workflows/:id/runs {session_id,
    input}` — or a trigger, whose author wrote the brief in advance for every
    fire (a webhook's payload rides along with it). What there is not is a
    bare button: nothing starts an execution without saying what it is about.
    The tool call's card is the execution's
    card: the task carries the `tool_call_id`, so the sequence's state follows
    the call in the transcript, as a spawned task's does. Which step runs next
    is the DEFINITION's answer, never the model's: a gate step reports a
    verdict, the edges decide.

31. **An execution's state keeps a log of every step LAUNCHED.** The task row
    names only the CURRENT step and run, so without the log a finished
    sequence could not say which turns belonged to which step, and a retry's
    second attempt could not be told from the first. `state.step_runs` records
    every `(step, run)` the launcher started, written by the launcher itself —
    under the run it is about to start, through the same `Advance` CAS, so it
    lands atomically with the row and a stop racing the launch loses the
    write, not the run. A run that never launched (a crash between the claim
    and the launch) is therefore not in it, which is the truth: the log is of
    launches, and the bounds — the lap bound reads its laps off it, the step
    ceiling (`MaxStepRuns`) counts its entries — count launches, because that
    is what costs. How each run ended is written when the driver decides what
    follows it — the last one IN the finalize itself (an ending
    `Continuation` carries the state into `Store.Finalize`), so the log and
    the task's terminal status are one write and cannot disagree.

32. **Delivery is a DEBT, not a call, and one waker owns it.** Background work
    finishes when its session may be busy, paused on a human decision, or gone
    with the process, so "session S is owed a turn carrying P" is a row
    (`wakeups`), drained at the moments a session becomes able to take one: the
    end of any run on it, and startup. One drain pays every debt the session
    has, so three results that landed while you were typing produce one turn,
    not three. Tasks are its one source — a workflow execution is a task —
    and every debt carries `inherit`, the configuration the turn runs under,
    snapshotted from the agent that ASKED (the spawning run's), so a session
    re-pointed at another agent mid-sequence still returns the result through
    the one that started it. A crash never loses a debt: the terminal write and
    its `wakeups` row land in ONE transaction (the store's `Finalize`).
    The SDK's task manager keeps no debt of its own — it reports endings
    through `OnFinished` and deliveries through `OnResultDelivered`, because
    when a session may be interrupted is a host policy. A task's debt is written
    where its terminal state is: the store's `Finalize` (and the restart sweep's
    `FailOrphans`) records the `wakeups` row IN THE SAME TRANSACTION as the
    status, so a crash can never leave a completed task whose parent is never
    told — a completed/failed task owes, a cancelled one does not. `OnFinished`
    then only DRAINS (pays now if it can); losing that call loses nothing, since
    the debt already exists and the next boundary settles it. The debt's inherit
    strips the task's own agent (`TaskAgentID`): the drain GROUPS debts by the
    inherit string, and a field the wake run never uses would split one turn
    into one per task agent. A debt born with no agent config is cancelled at
    the first drain — its inherit is frozen, so no boundary will ever do
    better. Wakeup rows match their session by ID ONLY (no generation column,
    unlike task rows — spec §2.13): the session delete cascade removes them in
    the same transaction, and both writers CAS on rows that cascade too, so a
    dead incarnation's debt cannot exist to be inherited. Single insurance,
    recorded deliberately.

33. **Plan mode is a restraint, so only a PERSON turns it on, and it belongs to
    the SESSION.** A session executes until somebody asks for a plan — a
    `/plan <message>` in the composer (offered when `/` is typed; nothing arms
    plan mode ahead of a message, the command is the message's; `/plan off
    <message>` is the way back out, an approved plan the other), riding on the
    run request's `plan` field (there is no phase endpoint; setting it and starting the run
    are one step, applied inside the run reservation so a busy-refused request
    never mutates the phase). It is not an agent setting and not something the
    model decides: the value of the gate is "a human looks before anything
    changes", and asking the model whether it needs looking at is asking exactly
    the wrong participant — a model that judges "simple, no plan needed" is the
    failure the gate exists to catch. `plan` absent leaves the phase alone, so a
    client that knows nothing of plan mode cannot knock a session out of it. An
    approved plan unlocks the SESSION rather than the turn, so the next message
    does not demand a second plan for work already agreed. The phase is a
    materialized `sessions.planning` column — read on every run and every
    session GET, so it is one row, not an O(n) scan of the entry log — written
    by the person's request and cleared by the approved `submit_plan` (that
    write is the durable unlock, and its persistence is the precondition for the
    run leaving the planning phase). A fork copies the column, so a branched
    session inherits the phase it forked in. The build is unconditional — every
    chat agent gets the gates and the phase decides whether they bite, because a
    resume rebuilds the agent AFTER the unlock and one built without
    `submit_plan` could not answer for the call the paused state names.

34. **A BACKGROUND run is built without plan mode or the task tools — and is
    TOLD that nobody is reading.** One flag, because
    all of it follows from the same fact: nobody is sitting in front of it.
    Plan mode is the one that deadlocks — `submit_plan` pauses for approval,
    and a background run's approval lands in a session the chat cannot open, so
    the sequence waits forever on a question nobody can see. Removing tools is
    only half of it: an agent still behaves like one in a conversation, asking
    for confirmation and stopping, and in a session nobody reads that is a
    deliverable nobody can answer. So the instructions say so, as a SUFFIX —
    the agent's own prompt may well tell it to ask. A run learns it is
    background from its session being a task's child (a workflow step's is —
    an execution is a task), and a lookup that FAILS is an error, not a "no" —
    reading it as a chat run is exactly how the deadlock happens.

35. **A step's approval is answerable from the conversation that asked.**
    `GET /sessions/:id/approvals` returns the approvals paused inside this
    session's tasks — a workflow step's included, tagged with the task they
    belong to — so the chat is the one approval surface; approve/reject is
    keyed by tool call id, so answering works from anywhere. The startup sweep
    and the approval reaper treat an execution as the task it is: one PAUSED
    on an approval is not an orphan and is left alone, one whose run died with
    the process is failed at the step it reached, and one whose approval
    expires unanswered is cancelled — otherwise the row stays working with
    nothing left that could finish it.

36. **A finished piece of background work leaves the transcript and enters the
    panel.** The turn a wake-up injects carries the notification prefix, and a
    prefixed user message never renders as a bubble — it is the model's input,
    not something the person said. What a reader gets instead is the Tasks
    panel, which holds tasks and workflow executions in ONE list — one list
    because they are one thing, tasks: work running in a session nobody is
    sitting in, reporting back the same way, stopped and retried the same way.
    The list is the socket's task state: the durable rows seed it and
    `task.updated` keeps it current — a workflow's status is the TASK's, told
    by that event, since its step runs end without ending it. Its detail lens
    is the child session's own transcript and trace, so drilling into a
    sequence shows every step in order.

37. **A step a person must approve is a PAUSE of the task, filed as an
    approval — not a run.** Reaching a `pause_before` step, the launcher keeps
    the turn the step will start with in the state, files a pending approval of
    kind `step` under the run id the row already claimed (its one "tool call"
    is `start_step`, naming the step), and marks the task `input_required` —
    the three in ONE transaction, so no moment exists with a task paused on
    nothing to answer or an approval answerable for a task still working. No
    run exists until the decision: approving reclaims the task (the same
    `input_required → working` CAS a tool approval takes, bound to that run
    id) and starts the step's run under it; rejecting stops the execution —
    cancelled, the person's decision, so nobody is woken. A decision is ONE
    transaction as well — the approval row deleted (the exclusive claim) and
    the task moved (`ClaimApprovalWorking` / `ClaimApprovalCancelled`), the
    reaper's expiry included — so of two racing decisions exactly one lands,
    a claim that does not hold writes nothing (the row stays for the retry of
    the decision), and no crash between the two halves can leave a task paused
    on nothing or answered while paused. Because it is a task
    pause and an approval row, everything a tool approval already has applies
    unchanged: it is listed on the parent's approvals and answered from the
    strip, the reaper expires it (cancelling the execution), the restart sweep
    leaves it alone, a stop discards it, and `task.updated` names the decision
    (`pending_call_id`) so a client can offer it without a run event to learn
    it from — and since the pause has no run stream to ride, that event is
    BROADCAST to every connection rather than published on the (nonexistent)
    run; the same fallback covers a step transition or a retry announced
    before its run registers. The one thing it deliberately is not is a `RunState`: there is
    nothing to resume, so the resume machinery never sees it (`Kind` says so
    before the state is read).

38. **The chat's session scope is THREE contexts, split by how often each
    moves — and what moves per streaming delta is in none of them.** A React
    context has no selectors: every consumer re-renders when its value
    changes, so one context holding the run's `streaming` text would re-render
    every finished turn of a long transcript on every delta. `ChatSessionState`
    (the run lifecycle: flips per run), `ChatActions` (callbacks: change on a
    session switch), `ChatTasks` (the task-derived lookups: change per task
    event) are memoized on their inputs in `ChatView`; `streaming`, `reasoning`
    and the live turn's `parts` stay props of the ONE live `TurnBlock`. A test
    pins it: a delta re-renders the live turn and nothing else. This is why the
    deep components (`TurnBlock` → `ProcessTimeline` → `ToolCallCard`, the
    strip, the Tasks panel) read the scope with `useChatSession` /
    `useChatActions` / `useChatTasks` instead of receiving it four levels down.

39. **A workflow definition the model writes lands only through an approved
    `save_workflow`, names steps rather than ids, and never reaches a
    background run.** Authoring is a WRITE to configuration, so its gate is not
    the model's to switch off: `save_workflow` carries `NeedsApproval` itself
    (like `submit_plan`), not through the agent's `approve_tools`, and its
    approval card is the review — the definition as the hub draws it and, on
    an update, the stored definition diffed line by line. The gate's other
    half is `NeedsApprovalFunc`: the same resolve the write does runs before
    anyone is asked, and a proposal that would not save — an unknown agent, a
    duplicate or reserved step name, an edge to no step, no description, a
    gate whose words collide — needs no approval and executes at once into a
    refusal the model reads; only a fixable fault skips the person, a store
    fault still asks, so no write ever lands unapproved. The model's shape has
    no ids: steps, agents and edges are NAMED (`bridge.workflowSpec`), the
    server owns the ids — assigning them, and on an update reusing the id of a
    step whose name (or, for a nameless one, its `Step N` as `get_workflow`
    reports it) is kept, which is what keeps a retry and an execution in
    flight naming the same step across a model's edit. Same name means the
    same workflow (`EqualFold`, as the unique index is `NOCASE`): a save is an
    upsert, and its result says which it did. Names being the handles, the
    STORE enforces them for every writer (`NormalizeWorkflow`: a step name is
    unique per definition, case-insensitively, and `end` is reserved), so a
    definition the hub saved is one the model can read back and edit; and the
    approval card lays the proposal out as the server would store it — trimmed,
    gate words as `Verdict` compares them, edges resolved to the step's own
    spelling — so its chart and its diff against the stored definition show
    the save, not the model's spelling of it. The pair is per-agent opt-in
    (`behavior.workflow_authoring`) — a save schema on every request of every
    agent would be paid for by agents that never author — and attached only
    where the task tools are, on a chat run: a background run has nobody to
    approve, and a step that could write definitions would be a sequence
    editing sequences. `get_workflow` is `ReadOnly`, so plan mode reads
    definitions and withholds saves; the Context panel meters the pair as its
    own bucket (`tools · workflows`).

**Configuration**

40. **A global setting is one entry in the registry, and everything else
    derives from it.** `internal/settings` names every key, its kind, its
    default and how the panel presents it; nothing may name a setting any
    other way. The backend reads through `settings.Reader` (which resolves the
    registered default, so no reader carries its own fallback), masking is
    `Kind == secret` rather than a hand-kept list, and the panel renders the
    table the server serves rather than a copy of it. The rule exists because
    the key set used to live in four places at once — backend literals, a
    masking map, the provider table, and a `DEFAULT_KEYS` array in the
    frontend — and `approval_ttl_minutes` is what that cost: read by the
    reaper, documented here, and invisible in the UI for want of a fifth edit.
    A default lives in the registry, never in a `const` beside its one reader:
    a default the panel cannot show is one the operator has to read the source
    to learn.

41. **A destructive action confirms once, in one place.** Every settings
    panel's Delete goes through `useCrud.remove`, which asks (Primer
    `useConfirm`) before calling the API — a new panel gets the guard by
    construction rather than by remembering to add it. Deletes that live
    outside `useCrud` (conversations, skill repos, background tasks, triggers) use the same
    `useConfirm` dialog, never `window.confirm` and never a bare button. The
    rule exists because the guard used to be per-panel and eight of ten
    destructive flows had none.

42. **Ownership is a column on sessions; everything else inherits or is
    shared.** There is no second owner column: a task's hidden session
    inherits its parent's owner at creation (`CreateOptions.ParentID`), a
    trigger's owner is its session's, an approval's is its session's. Shared
    configuration has no owner and is admin-written. A new per-user thing is
    either filed on a session or it is shared — not a third category with
    its own column and its own checks.

43. **Shutdown is ordered, and every waiter is told.** On SIGINT/SIGTERM:
    the clock stops (a tick during the drain would start a run the drain
    refuses); the maintenance loops stop (a reaper ticking during the drain
    could expire the approval being persisted); every running run is
    cancelled and waited for, so its partial turn persists; every run's
    broadcaster is closed — an interrupted run's too, which the drain neither
    cancels nor waits for — so an SSE stream returns; the WebSocket
    connections are closed with `1001 Going Away`, since a hijacked
    connection is outside `http.Server.Shutdown`'s reach; then the listener
    drains, for at most five seconds, and whatever it was still waiting on
    is a warning, not an exit status.

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

Every id column — primary keys, the foreign keys that reference them, and the
row ids of `entries` and `trace_events` — is typed `uuid` and holds a UUIDv7
(`store.NewID`): time-ordered, so inserts land at the right edge of an index
and rows created together sit together; 16 bytes a key on PostgreSQL. Order
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
| `sessions`          | Chat sessions; `owner_id` is the one ownership column (see [Ownership and roles](#ownership-and-roles)) |
| `entries`           | Session entries (the conversation, annotations and compaction checkpoints)          |
| `append_points`     | Where each session stands: branch tip + highest sequence number (see invariant 26)  |
| `agent_configs`     | Agent configurations                                                                |
| `mcp_servers`       | MCP server configurations                                                           |
| `memories`          | Agent memories                                                                      |
| `settings`          | Global key-value settings                                                           |
| `sandbox_configs`   | Sandbox configurations                                                              |
| `guardrails`        | Custom guardrail definitions                                                        |
| `trace_events`      | Trace spans (agent, generation, function, handoff, compaction) + run lineage; pruned in batches by `trace_retention_days` |
| `pending_approvals` | Runs paused for human-in-the-loop tool approval (persisted so they survive restart) |
| `tasks`             | Background tasks — sub-agents spawned via `spawn_task` and workflow executions (`kind`, `state`) — durable identity and status |
| `providers`         | Model-API endpoints and their credentials; agents reference one |
| `workflows`         | Fixed step sequences (each step: agent + prompt, with a stable id); an execution is a `tasks` row |
| `audit_events`      | Who did what, to what, when — see [Audit log](#audit-log)                          |
| `wakeups`           | "This session is owed a turn carrying this" — the debt background work leaves behind; settled rows are pruned after 7 days |
| `context_profiles`  | One row per session: what its last build put in front of the conversation (prompt layers, tool surface) |

The database file can be deleted and recreated freely — there is no migration
mechanism.

## Roadmap

- **Multiple instances.** Two processes on one PostgreSQL do not cooperate
  yet — not even a rolling restart: the truth about a live run is in process
  memory. What is process-local today, and how it breaks with a second
  instance: `RunHub` (a run is 404 everywhere but its instance; the
  one-run-per-session rule holds only by the entries unique index); the
  orphan sweep at startup fails every `working` task, including the other
  instance's; the `Waker` reads the local hub, so two instances can wake one
  session into concurrent runs; `ConnRegistry` broadcasts reach local
  connections only; the cron table is loaded per instance, so every schedule
  fires once per instance; the webhook replay guard, the OAuth pending-login
  and exchange maps, the MCP OAuth callback channel, refresh-token dedup, the
  sandbox instance cache and terminal fences, exec_command trust, and the
  rate budgets are all in-memory maps. Already shared through the database:
  pending approvals, wake-up debts, auth tokens, the audit log, ownership.
  The direction chosen: shard by user (sticky load-balancing on the user
  id), an `instance_id` with a heartbeat table and lease-based ownership
  instead of "is it in my memory", maintenance loops kept idempotent,
  SQLite refused for more than one instance — and, before any of it ships, a
  migration mechanism,
  because a rolling upgrade is two binaries on one schema.
- **Guardrail ordering at the approval gate.** The tool stages are configurable
  now (a guardrail's `stages` cover `tool_input` / `tool_output` for every tool
  call), but `RunOptions.PreApprovalToolInputGuardrails` is not exposed as an
  agent config field. With it on, a guardrail rejection resolves an
  approval-gated call without a human round-trip. Per-TOOL binding — "only this
  tool's arguments go through this guardrail" — is a separate thing the SDK
  does not model; it would need a `Stages`-like selector keyed by tool name.
- **Renderer hints on tool-call cards.** PROTOCOL.md F4 reserves a
  `display.renderer` hint ("terminal", "diff", "table") on the structured
  display projection. The card does not consume any such hint today: live
  progress renders as a `<pre>` regardless, and a finished result as plain
  text — a multimodal result (an image, a file: the Responses content list,
  see `run.tool_result`) is the one shape it renders by content rather than
  by hint. Wiring a renderer field end to end (SDK `ToolResult.Display` is a
  plain string today) — a terminal view for shell output, a diff view for a
  patch — is the remaining half of the streaming partial-results work.
