# Authentication — the workbench

REST requests authenticate with a Bearer token in the `Authorization` header:

- `Authorization: Bearer <token>`

A `?token=<token>` query parameter is not accepted for REST — a token in a URL
leaks into browser history and proxy logs. The WebSocket instead authenticates
at the application level, via its first message (see [WebSocket protocol](../reference/protocol.md#websocket-protocol)),
resolved by the same credential check as REST.

The auth surface under `/api/v1/auth`:

| Method | Path                             | Auth | Description                                              |
|--------|----------------------------------|------|----------------------------------------------------------|
| GET    | `/auth/config`                   | none | How to authenticate: `{mode, providers?}` — the login page renders from it |
| POST   | `/auth/login`                    | none | Validate the static token (token mode; 400 in OAuth mode) |
| GET    | `/auth/check`                    | yes  | The SPA's stored-credential probe: `{ok}` for a valid Bearer, `401` otherwise |
| GET    | `/auth/oauth/:provider/start`    | none | 302 into the provider's authorize flow (PKCE); sets the login cookie |
| GET    | `/auth/oauth/:provider/callback` | none | Provider redirect target; 302 into the SPA with `#auth_code=<one-time>` on success, `#auth_error=<tag>` on failure (`state_mismatch`, `cancelled`, `exchange_failed`, `not_allowed`, `disabled`, `login_failed`) |
| POST   | `/auth/exchange`                 | none | Trade the one-time code for `{token, user}` — the only response the session token's plaintext rides |
| GET    | `/auth/me`                       | yes  | The authenticated caller: `{id, email, name?, role, avatar_url?}` |
| POST   | `/auth/logout`                   | yes  | Revoke the presented session token (no-op in token mode) |
| GET    | `/auth/tokens`                   | yes  | List the caller's personal access tokens — labels and dates, never secrets |
| POST   | `/auth/tokens`                   | yes  | Mint a PAT — `{name, expires_in_days?}` (`0` = never expires); the plaintext (`ags_p_…`) rides this response only |
| DELETE | `/auth/tokens/:id`               | yes  | Revoke the PAT                                           |
| GET    | `/auth/audit`                    | admin | The audit log, newest first (`?limit` ≤ 500, `?before=<event id>`) |
| GET    | `/auth/user-labels`              | yes  | The id→person directory (`id`, `name`, `email`) that labels row owners |
| GET    | `/auth/users`                    | admin | Every account with its role and `disabled_at`            |
| PATCH  | `/auth/users/:id`                | admin | `{role?: admin\|member, disabled?: bool}`; never one's own account, never the local one; `409` when it would leave no enabled admin; disabling also revokes every token; answers `204` with no body |
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
scripts, CI) — the `/auth/tokens` routes in the table above. A PAT
authenticates everywhere a session token does — REST and the WS auth frame.
In token mode the endpoints answer 400: the static compare is the whole
check there, so a PAT could be minted but never authenticate.

### Ownership and roles

Four rules, enforced at the routes and handlers (`handler/authz.go`,
decisions §5.29), shape who may do what:

- **Scoped configuration carries two facts: who sees it, and who wrote it.**
  Agents, providers, MCP servers, skills and workflows carry
  `scope: private | global` (visibility) and `owner_id` (the author —
  permanent, kept across scope changes). Any member creates (the row lands
  private, theirs; claiming `global` on create is admin-only, `403`) and a
  foreign private row answers `404` like a missing one. Who may write:
  | Act | Who |
  |---|---|
  | Edit | the author (private *or* published) — plus an admin on any global row; never an admin on a member's private row (`403`) |
  | Delete | the author, or an admin (any row) |
  | Publish (`POST /<entity>/:id/scope` `{"scope":"global"}`) | an admin |
  | Unpublish (`{"scope":"private"}`) | an admin, or the author — the row returns to its author |
  | Transfer (`PUT /<entity>/:id/owner` `{"user_id"}`) | an admin — references are re-validated as the NEW owner (400 when they cannot see one); a provider's is also refused while agents would be stranded; a skill's moves its whole repo group |

  A flip naming the row's current scope answers `409`, as does one colliding
  with a name in the target namespace or a demote stranding a provider's
  referencing agents. The reference and name-resolution rules
  (own-over-global, global-references-global, runtime filtering) are spec
  §5.29's; **skills additionally namespace by repository** — see
  [skills](../reference/protocol.md#skills--apiv1skills). The model's tools follow suit —
  `save_workflow` rides every owner's run, a new name saving a private
  workflow while an existing global name stays an admin's to change — and
  signing a provider into ChatGPT is its owner's act.
- **Host configuration stays read-everyone, write-admin.** Sandboxes (the
  test and container endpoints included), settings, guardrails and memories
  change what runs on the host or whose host credentials are spent, so
  `POST`/`PUT`/`DELETE` answer `403` for a member. The web terminal follows
  project ownership — a member into their own project's container, an admin
  into any (see the
  [Terminal endpoint](../reference/protocol.md#terminal-endpoint--get-wsterminal)). And using
  configuration is not writing it: a member runs any agent, workflow or
  sandbox they can see, in their own session, approving their own tool
  calls. A shared sandbox is a shared shell — every member who can pick it
  executes on that host under the credentials the server stores. That is the
  single-workspace model — one team, one trust boundary — not an oversight.
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
  holds Settings, Sign out and — for admins — Admin, a dialog with a tab per
  plane: Members (roles, disabling, signing out everywhere); then Providers,
  Agents, MCP, Skills and Workflows, each listing every member's rows with
  their author, for publish/unpublish and transfer — never edit, which stays
  with the author under Settings; then Sessions (every owner's: reassign or
  delete, never read), Projects (newest first) and Audit logs. Settings for a member shows the scoped
  panels writable — their own rows editable, others' marked with their
  author and read-only — and the host panels (sandboxes, settings,
  guardrails, memories) read-only, plus their Account (profile and PATs).
  Nothing in the UI hides what the server would allow.

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
ChatGPT login callback is deliberately absent — its redirect lands on
`localhost` (never on this server), and the user pastes the resulting URL back
through the authenticated `/providers/:id/chatgpt/complete` route, which redeems
the code server-side ([decisions §5.41](../explanation/decisions.md#541-chatgpt-login-redeems-a-pasted-callback-url-not-a-loopback-listener)). Webhook triggers
(`POST /hooks/:id`) live outside `/api/` for the same reason a callback does —
the caller is another system, with no token — and are authenticated by HMAC
signature instead (see [Workflows](../reference/protocol.md#workflows--apiv1workflows)).
