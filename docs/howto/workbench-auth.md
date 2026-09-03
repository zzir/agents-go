# Authentication — the workbench

REST requests authenticate with a Bearer token in the `Authorization` header:

- `Authorization: Bearer <token>`

A `?token=<token>` query parameter is not accepted for REST — a token in a URL
leaks into browser history and proxy logs. The WebSocket instead authenticates
at the application level, via its first message
([WebSocket protocol](../reference/protocol.md#websocket-protocol)), resolved
by the same credential check as REST.

The routes live under `/api/v1/auth`; their shapes are in the OpenAPI
document. What a schema cannot say about them:

- `POST /auth/login` validates the static token and is `400` in OAuth mode;
  the `/auth/tokens` routes are `400` in token mode, where the static compare
  is the whole check and a PAT could be minted but never authenticate.
- A **plaintext credential is shown once**: the session token rides only the
  `POST /auth/exchange` response, a PAT (`ags_p_…`) only its create response.
  The OAuth callback redirects into the SPA with a one-time
  `#auth_code=…`, or `#auth_error=<tag>` (`state_mismatch`, `cancelled`,
  `exchange_failed`, `not_allowed`, `disabled`, `login_failed`).
- `PATCH /auth/users/:id` answers `204` with no body, refuses one's own
  account and the local one, and is `409` when the change would leave no
  enabled admin; disabling also revokes every token the account holds.
- `GET /auth/user-labels` is readable by every member (it is what labels row
  owners); roles and account state are admin-only.

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
- **`--token` is refused in OAuth mode** — programmatic access uses the
  personal access tokens below. The Google redirect URI to register is
  `<base-url>/api/v1/auth/oauth/google/callback`.
- The client secret can come from the environment instead of the flag
  ([configuration reference](../reference/configuration.md#environment-variables)).
  Secrets configure the process only; they are never stored in the database.

**Personal access tokens** (`/auth/tokens`) are OAuth mode's programmatic
credential for curl, scripts and CI: a PAT authenticates everywhere a session
token does, REST and the WS auth frame alike, and `expires_in_days: 0` never
expires.

### Ownership and roles

Roles are `admin` and `member`; the first OAuth account and
`--bootstrap-admin` sign in as admin, and in token mode the one local account
is an admin that owns everything, so every check passes. Who may do what to
which row — the scoped-configuration write matrix, session ownership, the
host-configuration plane and an admin's manage-never-read reach — is
[the wire surface's Authorization section](../reference/protocol.md#authorization),
enforced at `handler/authz.go` ([invariant 42](../explanation/workbench-invariants.md)).

Two consequences worth stating in the operator's terms:

- **A shared sandbox is a shared shell.** Every member who can pick one
  executes on that host under the credentials the server stores. That is the
  single-workspace model — one team, one trust boundary — not an oversight.
- **The UI hides nothing the server would allow.** Settings is one hub
  ([invariant 61](../explanation/workbench-invariants.md)): a member sees
  their own scoped rows editable, others' read-only with their author, the
  host panels read-only, and their Account. An admin's view adds an **All
  members** toggle on each scoped panel and an **Administration** group —
  Members, Sessions (reassign or delete, never read), Projects, Audit logs.

**Switching auth modes keeps the data and changes who can reach it.** Every
session made in token mode belongs to the local account, which OAuth mode
leaves dormant; every session made in OAuth mode belongs to the person who
made it, whom token mode cannot sign in as. After a switch the old sessions
are therefore visible only in the admin's `GET /sessions?all=true` listing —
the Settings hub's Sessions panel — where they can be reassigned
(`PUT /sessions/:id/owner`) or deleted.

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
  goroutine after the response. `POST /auth/login` and `POST /auth/exchange`
  are requests like any other — no credential precedes them, so the handler
  names the account that signed in as the actor (`SetAuditActor`).
- **An act that is not a request**, named by what it is:

  | Action          | Who is the actor                        | Resource          | Detail                          |
  |-----------------|-----------------------------------------|-------------------|---------------------------------|
  | `ws.run.create` | the connection's user                   | session id        |                                 |
  | `ws.approval`   | the connection's user                   | tool call id      | verdict, scope, tool            |
  | `terminal.open` | the connection's user                   | sandbox id        | `project <id> (owner <id>)`     |
  | `workflow.save` | the session's owner (who approved it)   | workflow id       | `tool=save_workflow created`    |
  | `trigger.fire`  | the owner of the session it fired into  | trigger id        | `source=cron\|webhook started=` |

  A person's manual fire (`POST /triggers/:id/fire`) is the request's line;
  the clock's and a webhook's have no request, so the scheduler writes
  theirs. A `save_workflow` is the one configuration write that happens
  through a tool, so the tool writes it — the approval line alone would read
  like any other `exec_command`.

Retention is the process's `--audit-retention-days` (default 0 = keep
forever), deliberately a flag and not a setting — the log of configuration
changes must not be shortened through the API it records
([invariant 54](../explanation/workbench-invariants.md)). It carries each
actor's email and client IP, so deleting the database file is what deletes
that personal data. Admins read it at `GET /auth/audit` (`&before=<event id>`
pages older — the id is a UUIDv7, so it orders like the time and never ties)
and in the Settings hub's Audit logs.

Exempt from auth: the MCP OAuth redirect callback
(`GET /api/v1/mcp-servers/oauth/callback` — the browser follows it without an
Authorization header), the OpenAPI document (`GET /api/v1/openapi.yaml`), the
`config` / `login` / `exchange` and `oauth/*` routes above, and `GET /health`.
The ChatGPT login callback is deliberately absent: its redirect lands on
`localhost`, never on this server, and the user pastes the resulting URL back
through the authenticated `/providers/:id/chatgpt/complete` route
([decisions §5.41](../explanation/decisions.md#541-chatgpt-login-redeems-a-pasted-callback-url-not-a-loopback-listener)).
Webhook triggers (`POST /hooks/:id`) live outside `/api/` for the same reason a
callback does — the caller is another system, with no token — and prove
themselves by HMAC signature instead
([Workflows](../reference/protocol.md#workflows--apiv1workflows)).
