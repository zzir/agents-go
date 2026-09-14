# The workbench's wire surface

What `agents-server` exposes over HTTP and WebSocket, and what each call
*means* — the semantics a schema cannot state.

**Endpoint and payload schemas are not repeated here.** They are generated from
the handlers into the OpenAPI document, which CI diffs on every change; a
running server serves it at `GET /api/v1/openapi.yaml` (unauthenticated — see
[OpenAPI](#openapi)). This page carries what that document cannot: which calls
are refused and why, what a write does to rows it does not name, and how two
endpoints compose. The panel and handler rules behind those answers are the
[workbench invariants](../explanation/workbench-invariants.md); the reasoning
is [decisions §5](../explanation/decisions.md).

What is still moving — the two WebSocket changes agreed but not shipped, and
the known gaps in what ships — is the last section, [Open changes](#open-changes).

---

## REST API

Base path `/api/v1` — the only mount; there is no unversioned alias. All
request and response bodies are JSON. Request bodies are capped at 1 MiB
(matching the WebSocket frame limit): a declared length past it answers
`413`, an undeclared body is cut there and fails its decode. Two routes carry
more: `POST /playground/generate` replays a stored span payload, so its cap
is 64 MiB, and `POST /attachments` takes an image, so its cap is the 10 MiB
image limit plus multipart slack. API responses of 1 KiB and more go out
gzip-compressed to a client that sends `Accept-Encoding: gzip`; a run's event
stream (`GET /runs/:id/events`), the replay stream, the UI's assets
(pre-compressed at build) and WebSocket frames are never compressed by the
server.

### Errors

Every non-2xx response is `{"error": {"code": …, "message": …}}` — `code` a
stable machine-readable identifier, `message` human-readable detail.

| Code           | HTTP | Meaning                                                                       |
|----------------|------|-------------------------------------------------------------------------------|
| `validation`   | 400  | Malformed request body or invalid parameter                                   |
| `unauthorized` | 401  | Missing or invalid Bearer token                                               |
| `forbidden`    | 403  | Refused for who the caller is: admin role required, the row belongs to another user, or a member writing admin-only configuration |
| `not_found`    | 404  | No such resource                                                              |
| `conflict`     | 409  | Resource is in the wrong state for the request                                |
| `upstream`     | 502  | A failing upstream dependency (model provider, MCP server, sandbox host, git) |
| `internal`     | 500  | Unexpected server error (detail is logged, not returned)                      |
| `unavailable`  | 503  | Transient refusal: the server drains for shutdown, or the credential could not be checked against the store — retry later |
| `rate_limited` | 429  | This client IP exceeded an endpoint's rate budget — slow down and retry       |

### Response conventions

- **Create** returns `201 Created` with the created resource.
- **Update** (`PUT`/`PATCH`) returns `200 OK` with the full updated resource —
  except `PATCH /auth/users/:id` and every `PUT /<entity>/:id/owner` transfer
  (sessions included), which answer `204` with no body.
- **`PUT` is a full replace**: a field omitted from the body is written as its
  zero value, not kept, so a client must send every field the resource should
  retain. The one exception is a secret field, where the mask sentinel keeps
  the stored value (see [Secret handling](#secret-handling)). `PATCH`
  (sessions, users) applies only the fields present.
- **List** returns `[]` for an empty collection, never `null`.
- **Delete** returns `204 No Content`.
- A write (update or delete) against a missing resource returns `404`.
- Secret fields are write-only — see [Secret handling](#secret-handling).

### Authorization

Every request acts as one account: in token mode the single local account, an
admin that owns everything; in OAuth mode the signed-in person, `admin` or
`member` ([authentication](../howto/workbench-auth.md)). Three ownership
schemes decide what that account may do
([invariant 42](../explanation/workbench-invariants.md); the reasoning is
[decisions §5.29](../explanation/decisions.md#529-configuration-is-scoped-per-row-private-to-its-owner-or-global)).

**Scoped configuration** — agents, providers, MCP servers, skills, workflows —
carries `scope: private | global` (who sees it) and `owner_id` (who wrote it:
permanent across scope flips, moved only by a transfer). A foreign private row
answers `404` and is absent from listings, so scope is not an existence oracle;
a global row the caller may see but not touch answers `403`.

| Operation | Author | Other member | Admin |
|---|---|---|---|
| Read, list | own rows and every global row | global rows only; a foreign private id is `404` | every row |
| Create | `201`, lands private | — | `201`; `scope: global` in a create body is admin-only (member: `403`) |
| Edit (`PUT`) | `200`, private or published | `403` global / `404` private | `200` on any global row; a member's private row is `403` |
| Delete | `204` | `403` global / `404` private | `204` on any row |
| Publish — `POST /<entity>/:id/scope {"scope":"global"}` | `403` | `403` / `404` | `204`; `409` when already global or the name is taken among global rows |
| Unpublish — `{"scope":"private"}` | `204`; the row returns to the author | `403` / `404` | `204`; `409` when already private, the name is taken in the author's namespace, or a provider is still referenced by global or foreign agents |
| Transfer — `PUT /<entity>/:id/owner {"user_id"}` | `403` | `403` | `204`; `400` for an unknown account or a reference the new owner cannot see; `409` when the name is taken in the target namespace or a provider's move would strand agents |

- **References follow visibility** (`RefVisible`): a private row may reference
  global rows and its owner's private rows, a global row only global rows — a
  save, publish or transfer that breaks it is `400`. It is checked at write
  time where a dangling reference breaks the holder (an agent's provider and
  handoffs, a workflow's step agents); the advisory sets — MCP servers,
  skills — are filtered at agent build instead, so one shared config yields
  each member their visible subset.
- **Names are unique per visibility context**, by partial unique indexes:
  among global rows, per owner among private ones (skills add the import
  source, [§5.31](../explanation/decisions.md)). Every name a run resolves is
  own-over-global, and owning the global row is not shadowing
  (`store.Shadows`): its author still gets a private row of that name.
- **Listings order by AUTHORSHIP**, not scope: for agents, providers, MCP
  servers and workflows a member sees others' shared rows first, then their
  own, each newest first (`created_at DESC`, id the tiebreaker); an admin sees
  the table ungrouped, newest first. Skills group by repository, published
  groups first. `GET /auth/user-labels` (id, name, email), the admin panel's
  owner directory, is admin-only like the user list.
- **The write re-checks what authorized it.** Every scoped mutation carries
  the `(scope, owner)` pair into its write and answers `409` when a transfer
  or flip landed in between. A direct DB write that leaves scope empty lands
  private (`stampScope`); the API always states both.
- **The model's tools follow the same contract.** `save_workflow` rides every
  owner's run: a new name saves a private workflow, an existing GLOBAL name is
  an admin's to change and a member's save answers with guidance text, not an
  error. ChatGPT login and logout follow the provider's editability; skills
  flip per repository group — [Skills](#skills--apiv1skills).

**Session content** — `/sessions/:id` and everything filed on a session
(`/runs/:id`, `/tasks/:id`, `/approvals/:tool_call_id`, `/triggers/:id`) —
answers to `sessions.owner_id`, the one ownership column, and is the owner's
alone: a foreign id is `404`, listings are the caller's, a
workflow or trigger starts only into a session the caller owns, and run events
reach the owner's connections only. An admin manages, never reads:
`GET /sessions?all=true` lists every owner's sessions, `DELETE /sessions/:id`
and `PUT /sessions/:id/owner` work on any; opening, reading or running one
does not.

**Host configuration** — sandboxes (test and container routes included),
settings, guardrails, global memory — is read by everyone and written by
admins: a member's `POST`/`PUT`/`DELETE` is `403`. An agent's memory follows
the agent's edit rule and a session's memory is its owner's
([Memories](#memories--apiv1memories)). Using configuration is not writing
it: a member runs any agent, workflow or sandbox they can see, in their own
session, approving their own tool calls.

**Projects** have no scope column at all: they are per user, and the web
terminal follows the project — [Projects](#projects--apiv1projects),
[Terminal endpoint](#terminal-endpoint--get-wsterminal).

### Sessions — `/api/v1/sessions`

`POST /sessions` accepts an optional `agent_config_id`; it must reference an
agent VISIBLE to the caller — a foreign private id reads as absent and answers
`400`. Rename and pin are one `PATCH /sessions/:id` with a partial body.
`project_id` is written by the session's first project-carrying run and is
immutable afterwards, over the API included
([invariant 27](../explanation/workbench-invariants.md)): switching projects
means starting (or forking into) another session.

`fork` copies the source session's entries (and their traces) into a new
session, bounded by the optional `message_id` (`exclusive: true` stops before
it; an id that is not an entry of the source is `404`). Entry ids and parent links are rewritten into the fork's namespace, and
the fork inherits `agent_config_id` and `project_id` with no fresh bind.

`branch` moves the session's active branch to an entry, so the next run
continues from there. It APPENDS a leaf entry rather than deleting anything —
the abandoned attempt stays recorded (`on_path: false`), which is what makes
"regenerate" a "2 / 3 ‹ ›" switcher rather than a list of `(regen N)` copies.
Regenerating is `branch` back to the user's message followed by a run with an
EMPTY input: nothing to add, history to answer.

`/sessions/:id/messages` returns **session entries** — the SDK's
`session.Entry` as the runner wrote it, plus the row id the cursor pages on;
update entries are folded into their targets server-side, so a client never
applies them itself. `/sessions/:id/runs` is the server's walk over every
entry — one row per run with the question that started it — which is how a
trace card whose exchange is not on screen is labeled.

`/sessions/:id/context` reports what the session's ACTIVE branch occupies of
its model's context window, recomputed per call — there is no live event; the
panel refetches when a run ends. Compacted and off-path entries keep their
usage but leave the window figures, since the model no longer sees them; which
ruler each figure is on is
[invariant 28](../explanation/workbench-invariants.md). `prompt` is read from
the `context_profiles` snapshot the runner writes per run, and MCP tools are
asked live (bounded, 2s) — a server that cannot answer is reported with
`unavailable: true` on its own bucket, its count unknown rather than zero.

`POST /sessions/:id/compact` is one compaction pass through the run path's
own construction, skipping only the threshold — the kept window, the
pairing-safe split and the summary-of-summary guards still apply, which is
what `200` with `compacted: false` means. With the agent's `compaction_mode`
at `reset` or `hybrid` the same call, the threshold, and the model's own
`new_context` run one reset pass instead
([invariant 65](../explanation/workbench-invariants.md)): the checkpoint
carries the session memory and answers `reset: true` on the entry.

`/sessions/:id/memory` lists the session's own memory, the keys and sizes the
model (or its owner) wrote, and `/sessions/:id/memory/*key` reads one in
full. A row is deleted or edited through `/memories/:id`
([Memories](#memories--apiv1memories)).

**Pagination** — `messages` and `traces` accept `?limit=` and `?before_id=`.
Without `limit` the full list comes back oldest-first; with it, the newest
`limit` items — page backwards by passing the smallest id you received as
`before_id` (an exclusive upper bound; row ids are UUIDv7 and order by
insertion). For `messages` the limit counts the ENTRIES a client receives, not
table rows: update entries are folded first, so a page is never short of what
was asked for.

### Runs — `/api/v1/runs`

REST and the WebSocket share one run hub, so a run started over either is
observable over both, and **runs execute server-side, independent of the
connection that started them** — a dropped client or a page reload does NOT
cancel the run; reconnect and resubscribe (`GET /runs/:id/events` or the
WebSocket `run.subscribe`) to pick the stream back up without loss.

`POST /sessions/:id/runs` answers `201` as the run starts; `Prefer: wait=N`
([RFC 7240](https://www.rfc-editor.org/rfc/rfc7240)) trades that for the
outcome, and there is no unbounded form — N is capped at ten minutes, a
longer wait being the events stream's job. An `interrupted` outcome is a
pause, not an end: the decision resumes the SAME run id, continuing its event
sequence. `plan` enters or leaves the planning phase with this run; absent
leaves the phase as it stands
([invariant 33](../explanation/workbench-invariants.md)).

The first run that carries a `project_id` binds it to the session for good
([invariant 27](../explanation/workbench-invariants.md)): validated first —
a project that is missing or not the caller's is `400` and binds nothing —
and announced once with `session.project_bound`; from then on the bound value
wins over whatever the client sends, and a session with no project gets no
sandbox tools at all.

`GET /runs/:id` carries `task` only for a background task's run, with the
parent linkage its `run.started` event carries. Finished runs stay queryable
and replayable for **15 minutes** after they end, then `404` — the
session's transcript is always in `/sessions/:id/messages`.

`GET /runs/:id/events` is plain HTTP Server-Sent Events (unrelated to MCP's
deprecated SSE transport), the `id:` of each event its hub sequence number.
Besides a final event, the hub tearing the run's subscription down (retention
passing, a shutdown) closes it, so it never outlives the run. Event payloads
mirror the WebSocket [server→client events](#server--client).

`POST /runs/:id/cancel` stops a run: `?mode=graceful` lets the current turn
finish and stops before the next, the default aborts mid-turn — `204` either
way, `404` for a run the hub no longer holds, and a run paused for approval is
abandoned by either mode ([Approvals](#approvals--apiv1approvals)). The
WebSocket's `run.cancel` is the same call.

Start a run and stream it with plain curl (token from server startup):

```bash
TOKEN=...; H="Authorization: Bearer $TOKEN"; BASE=http://127.0.0.1:9527/api/v1
SID=$(curl -s -H "$H" -X POST $BASE/sessions -d '{"name":"cli"}' | jq -r .id)
BODY='{"input":"hello","agent_config_id":"<agent-id>"}'   # + "attachment_ids":[…] for images
RUN=$(curl -s -H "$H" -X POST $BASE/sessions/$SID/runs -d "$BODY" | jq -r .run_id)
curl -N -H "$H" $BASE/runs/$RUN/events          # stream until run.output

# or fire-and-wait in one call, for up to a minute:
curl -s -H "$H" -H "Prefer: wait=60" -X POST $BASE/sessions/$SID/runs -d "$BODY" | jq .final_output
```

### Approvals — `/api/v1/approvals`

A pending decision is **persisted, so it survives a server restart**, and a
decision resumes the run over whichever transport is listening — REST and the
WebSocket share the hub. A decision on a session that already has an active
run is `409`. Unanswered approvals expire after `approval_ttl_minutes`: the
record is dropped and an error annotation written to the session, so the
timeout is visible.

**A newer message wins over a pause.** A run paused for approval is
abandoned when the session takes a new message (`POST /sessions/:id/runs`,
`run.create`) or when the paused run is cancelled (`POST /runs/:id/cancel`,
`run.cancel`): its `pending_approvals` row is deleted, the calls it was
waiting on are recorded as not run with the reason, and `run.cancelled`
carries that reason — `superseded` or `stopped`; a decision arriving after
that is `404`. Only a person's message abandons: a wake-up or a task's start
leaves the pause standing, and a background task's paused run is its task's
to stop.

**exec_command session approval.** An agent whose `approve_tools` includes
`exec_command` gates each shell command through a per-session trust store
that the approve body's `scope` (REST and `tool.approve` alike) widens:
`once`, `same` or `all`. Trust is in-memory and per session — it survives
interrupt/resume and resets on restart — and matching is exact, so approving
`go test` never green-lights `go test && rm -rf`.

### Tasks — `/api/v1/tasks`

A task is one piece of background work started from a session through the ONE
tool that starts any: `spawn_task` — a sub-agent on a prompt, or, told a
`workflow` name, a workflow execution (`kind: "workflow"`, see
[Workflows](#workflows--apiv1workflows)). Each runs on its own hidden session
and reports back by injecting a notification into the parent session (the
SDK's [background tasks](../howto/tasks.md)). Status uses the MCP Tasks
five-state vocabulary, read live from the hub for a running task and from the
store after it ends.

A `retry` counts against the session's live-task cap
(`max_tasks_per_session`) like a spawn and queues behind it the same way; a
task that used every attempt (3 by default) is `409`. `GET /sessions/:id/tasks`
is one session's, newest first; `GET /tasks` pages every live session's, each
row with its session's name — `?live=true` is the `working` /
`input_required` rows, `?kind=workflow` the executions.

### Agents — `/api/v1/agents`

An agent config is the top-level scalars, the knobs as **grouped nested
objects** (`behavior`, `resilience`, `guardrails`, `session`, `approval`,
`compaction` — each group one JSON column, so a new knob needs no schema
change) and the top-level JSON blobs. **The list fields are JSON arrays**
(decisions §5.67): `tools` (MCP server ids), `handoffs` (agent ids),
`approval.approve_tools` (tool names, or `["*"]`) and `skills` (skill ids) —
`skills` is the one whose absence means something: `null`/omitted gives the
agent every skill its scope can see, `[]` none. Beyond the shape, a write checks:
`avatar` is a path into the UI's built-in catalog (anything else, an external
URL included, is `400`); a `resilience.fallback_models` entry is
`{provider_id, model}` — the provider must exist and be one the agent may
reference (re-checked as the row is written, and a fallback entry holds the
provider like a primary would: its delete, unpublish and transfer are refused
while the agent stands), an `api_key` or an unknown key in an entry is `400`
(decisions §5.69); an entry stored before `provider_id` reads back with the endpoint it
named (`provider_type`, `base_url`, read-only, never its key) and resolves to
a provider at that endpoint when the run builds; an `error_handlers` entry's
`final_output` is a string for a plain-text agent or matches `output_schema`
for a structured one. With compaction enabled, a context-overflow error from
the provider also triggers a FORCED pass and the turn retries from the shrunk
history (spec §2.5g) — the threshold predicts, this reacts.

Plan and todo mode are NOT agent settings: `todo_write` is on every chat
agent, and plan mode rides on the run request (`plan`) and belongs to the
session, which reports it as `planning`
([invariant 33](../explanation/workbench-invariants.md)); workflow authoring
IS one, `behavior.workflow_authoring`, off by default
([invariant 39](../explanation/workbench-invariants.md)).
`behavior.override_system_prompt` sends the agent's `instructions` alone —
the `system_prompt` setting is not prepended, even when they are empty
([invariant 67](../explanation/workbench-invariants.md)).

`GET /agents/:id/tools` is the agent's CURRENT surface — the built-ins,
connected MCP servers' tools (a server whose listing fails is skipped, not
fatal) and the skills reader — as a member's own runs would get it; it backs
the Replay dialog's tool picker. An agent body carries no model-API
credential — it names a provider, which is where the key lives
([providers](#providers--apiv1providers)).

### MCP Servers — `/api/v1/mcp-servers`

The one transport is streamable HTTP (decisions §5.25), so `config` is
`{endpoint, headers, auth_mode, oauth_*, max_retry_attempts, retry_backoff_ms,
use_structured_content}` with `auth_mode` `header` or `oauth` — a raw JSON
blob the OpenAPI document cannot expand. `max_retry_attempts` retries a failed
`list_tools`/`call_tool` (`0` never, `-1` without limit), `retry_backoff_ms`
is the base of its exponential backoff (`0` the SDK's 1s), and
`use_structured_content` takes a result's `structuredContent` alone, for a
server that fills only that field. A local stdio-only MCP server can join
through a stdio→HTTP proxy such as `mcp-proxy`.
Enabled servers are connected automatically on startup and after
create/update; `POST /mcp-servers/:id/connect` is the manual connect (the
Connect button), which for an OAuth server may answer with an `authorize_url`
instead of a connection. Disabling disconnects, and a disabled server cannot
be connected (`409`) — agents pick tools by live connection, so the toggle is
a hard off switch. `GET /mcp-servers/:id/tools` lists what a connected server
exposes; a server that exists but is not connected is `409`, unlike the `404`
of one the caller cannot see.

Every read endpoint reports one derived `status` per server: `disabled`,
`connecting` (handshake in flight), `authorizing` (OAuth popup pending user
action), `needs_auth` (OAuth without a saved token — connect returns an
authorize URL), `disconnected` (enabled but no live connection), or
`connected`. Writes reconnect in the background, so the status a PUT/POST
returns is often still `disconnected` or `connecting` — poll the list until it
settles. While `authorizing`, calling connect again is safe and intended: it
supersedes the stale attempt (the user closed the popup, which sends no
signal) and returns a fresh authorize URL; an abandoned attempt expires after
5 minutes. The flow logs each step —
[MCP OAuth troubleshooting](../howto/mcp-oauth-troubleshooting.md) reads them.

A grant is persisted with its refresh context and refreshed in place, so a
disable/enable cycle or a restart needs no re-authorization
([invariant 11](../explanation/workbench-invariants.md)). Only a rejected
refresh token falls back to interactive authorization: in-flight tool calls
fail fast with a re-authorize message, and the next connect returns an
authorize URL. The `oauth-token` DELETE endpoint (the edit form's "Clear
auth") drops the saved grant, e.g. to re-authorize with a different account.
The secret-bearing config fields come back masked
([Secret handling](#secret-handling)).

### Settings — `/api/v1/settings`

The keys, their types, defaults and how the panel presents them come from ONE
registry, served at `/setting-defs`
([invariant 40](../explanation/workbench-invariants.md)); the keys are tabled
in the [configuration reference](configuration.md#runtime-settings). A write
names a defined key and carries a value its kind accepts, or it is `400`; an
EMPTY value is always accepted — that is how a setting is returned to its
default. Reads are laxer than writes on purpose: `GET /settings` lists a key
the registry no longer defines with `"unknown": true` and its value masked
(whether it WAS a secret is unknowable once the def is gone), and `DELETE`
takes it, so a value left behind by an older build can be seen and cleared.
The `storage` keys (the attachment bucket) are the exception: they are written
as one section through `PUT /attachments/storage` and `PUT /settings/:key`
refuses them one at a time — see
[Attachments](#attachments--apiv1attachments).

### Server info — `/api/v1/server` (read-only)

The process facts a client is subject to but cannot change: the build version;
the server's local time zone (an IANA name — cron triggers tick in it, so a
client showing a schedule or a `next_fire_at` can say which zone it means);
and whether a credential-sealing key is in force, so a panel can say that
stored secrets are plaintext ([Secret handling](#secret-handling)). Caps are
settings, not reported here.

### Skills — `/api/v1/skills`

A skill is one stored `SKILL.md` document (decisions §5.26): `content` is the
input, `name` and `description` are read from its frontmatter at save time.
The model discovers skills through an index in the agent's instructions and
reads one on demand with `read_skill`; an agent's `skills` selection (skill
ids) restricts both. A skill's model-facing name carries its repository
(`owner/repo:name` for a GitHub import, `host:name` for another URL, the bare
name for one authored here), and uniqueness is per `(repo_label, name)`
within a visibility context ([decisions §5.31](../explanation/decisions.md)) —
a collision only happens within one repository, or between two sources
reducing to one label, and the second file is skipped with a reason.

**A repository's skills move as one group**, named `(repo, owner)` and never
searched for. Scope flips all or nothing through `POST /skill-repos/scope`
with `{repo, scope, owner_id?}` — the owner defaults to the caller's, and
`owner_id` is how an admin promotes a member's import; `404` for a group
nobody has, `409` when it is already in that scope or a name collides in the
target one. `POST /skills/:id/scope` serves workbench-authored rows, each its
own group, and is `400` on an imported skill. Ownership moves the same way:
`PUT /skills/:id/owner` on any row transfers the whole group in one
statement, `409` when the new owner already holds a group for that repository
(merging two is how a mixed-scope pile forms).

`POST /skill-imports` with `{url, owner_id?}` upserts skills from elsewhere —
a GitHub repository walked through its API (every `SKILL.md` at any depth,
pinned to the HEAD commit) or one raw `SKILL.md` at any other URL; the server
never runs git. A sync refreshes exactly
the named group — naming another owner is admin-only (`403` for a member) and
targets that owner's PUBLISHED group, so a member's private group, or one
that does not exist, is `404`; a first import may only create the caller's
own. Each fetch has a 30-second timeout and the whole import a five-minute
budget, connect through body read; a failed fetch answers `502`, and files
past an expired budget land in `skipped` with the deadline error rather than
being dropped silently. The response names the `repo` and lists what was
`created` / `updated` / `unchanged` / `skipped` (each skip with its reason);
`truncated` reports that GitHub's tree listing was cut off — files past the
cut were not seen at all.

Nothing is written during the fetch: the documents are collected (bounded by a
total-size budget as well as the per-document cap) and written in ONE
transaction that re-reads the group under lock and answers `409`, nothing
written, when its `(owner, scope)` changed since the caller resolved it.
Re-importing refreshes rows that were not edited locally; editing an imported
skill **detaches** it, and a detached skill is never overwritten. A sync's NEW
files inherit the group's scope and owner, so a published repository never
splits itself on an upstream addition. Documents are capped at 256 KiB,
imports at 200 skills per repository. In the UI the visibility badge sits on
the repo group's heading rather than each row, and who owns which group is the
Settings hub's Skills panel in its All view.

### Providers — `/api/v1/providers`

One configured endpoint and the credential that reaches it; `auth_mode`
`chatgpt_login` is openai-only. Agents REFERENCE a provider by id; nothing
else stores a model-API key, so this is the one surface a credential crosses
(masked on read; a mask round-trips only to the destination it was stored for
— [Secret handling](#secret-handling)). The ChatGPT OAuth flow lives here for
the same reason — every agent pointed at the provider shares one login
([ChatGPT OAuth](#chatgpt-oauth)). `GET /provider-types` (read-only) lists the
registered backends as machine facts — `type`, `auth_modes`, `unsupported`
request features — from the registry validation and construction derive from,
so the UI's capability hints cannot drift from what the build enforces.

### Workflows — `/api/v1/workflows`

A workflow is a FIXED, ordered sequence of steps run on ONE session; which
step runs next is the definition's answer, not the model's (a model-chosen
next agent is a handoff). An execution IS a background task — `kind:
"workflow"` in the tasks table and API — whose runs are the steps
([invariant 29](../explanation/workbench-invariants.md)): no second execution
table, no second set of endpoints. Each step is an ordinary run on the
execution's session, tools and handoffs included but the task and workflow
tools withheld ([invariant 34](../explanation/workbench-invariants.md)), and
the transcript is the data flow: later steps read what earlier ones did.
The [how-to](../howto/workflows.md) walks defining and running one.

An execution starts only with a brief written by someone who read the
session — the agent (`spawn_task(workflow=name, input)`), a person
(`POST /workflows/:id/runs {session_id, input, project_id?}`) or a trigger
([invariant 30](../explanation/workbench-invariants.md)). The brief LEADS the
first step's turn and is not repeated afterwards; it is kept in the task's
`state` beside a SNAPSHOT of the definition, so editing a workflow never
steers an execution in flight. A session busy with a run, or at its
background-task cap, refuses the start with `409`. A start no run asked for
leaves a people-only annotation where the tool call would have been
(`display.kind` `workflow_started`, with the task id, the workflow, the brief
and who started it), which is what labels the result's wake-up run in the
trace panel (`▶ build (cron @daily)`).

**Edges and laps.** `on_success` / `on_failure` name a step id or `end`;
naming an EARLIER step is how a sequence loops. One execution may take the
same backward edge `budget.max_laps` times (default 3): the transition that
would take it once more ends the execution failed, naming the edge (`loop
bound reached: verify → exec looped 3 times`); whatever the shape, an
execution stops after `MaxStepRuns` (50) step launches, retries included, and
a retry past either bound is refused before a run. A handler step's turn is
LED by the error it is handling — a failed run leaves no usable account of
itself in the transcript.

**Gate verdicts.** Without `gate`, failure is structural — the step's run
errored. With it, the step's verdict chooses the edge: the last non-empty
line of its output must be the pass sentinel (`PASS`, or the gate's own word)
for `on_success` or the fail sentinel (`FAIL`) for `on_failure`; the
instruction to end with one is appended to the step's prompt. A step whose
agent answers in structured output carries the verdict as a field instead — a
JSON object (bare or fenced) with a boolean `passed`, or a
`verdict`/`result`/`status` equal to a sentinel. A gate that reports neither
ends the execution failed, saying so — a check that forgot to report is a
broken step, not a coin flip — and a `FAIL` with no `on_failure` fails the
execution too. `state.step_runs` records how each step's run ended
(`completed`, `failed`, `pass`, `fail`), the last one in the same finalize as
the task's terminal status ([invariant 31](../explanation/workbench-invariants.md)).

**Pauses, compaction, budget.** A `pause_before` step holds the sequence as a
task pause filed as an approval; rejecting cancels the execution
([invariant 37](../explanation/workbench-invariants.md)). A `compact_before`
step folds the transcript first with the step's OWN agent's compaction
settings — an agent whose compaction is off leaves the transcript as it is,
logged. Every `budget` bound (zero = none; `max_laps` zero = 3) is checked
when the driver is about to launch the next step and again before a retry,
never mid-run: over any bound the execution stops, failed with the reason
(`budget exhausted: 4 of 3 steps`).

Only a FAILED execution retries — re-running a completed or cancelled one
would repeat its side effects; a retry re-runs the step the execution stopped
at, its turn the retry prompt followed by the step's own prompt again, gate
instruction included. A restart fails whatever was running, at the step it
reached, and deleting a session stops its tasks first, executions included.
Authoring from the chat (`get_workflow` / `save_workflow`) is per-agent
opt-in — [invariant 39](../explanation/workbench-invariants.md).

**Triggers** start work with no session asking: `kind: cron` on a
schedule, `kind: webhook` when something POSTs to `/hooks/:id` (outside
`/api/v1`). `target: workflow` fires the same start `POST /workflows/:id/runs`
makes, into the trigger's `session_id`; `target: agent` sends the brief as a
MESSAGE of that session, run by that agent under the session's own
sandbox binding, with a `trigger_fired` note before it. Either way the brief
is the author's, written in advance, and a webhook's body (up to 64 KB) is
appended to it as the payload. A session busy with a run, or at its cap,
refuses — that refusal is what the trigger shows as `last_error`. Cron ticks
missed while the process was down are not replayed. Deleting the session,
the workflow or the agent a trigger fires deletes the trigger. Triggers are capped at 50 per owner (`409`
above it), and the [how-to](../howto/workflows.md) has the cron syntax and a
signing example.

A webhook proves itself by signature, not token: `X-Timestamp` (UNIX seconds,
within five minutes of the server's clock) and `X-Signature-256` = hex
HMAC-SHA256(secret, `timestamp + "." + body`), a `sha256=` prefix accepted; a
bad or stale signature is `401`. The secret is minted at creation and shown in
that response only; `POST /triggers/:id/rotate-secret` mints another and the
old one stops working the moment the rotation answers (`400` on a cron
trigger). A delivery fires ONCE: the same timestamp and body sent again inside
the window — a sender's retry, a captured request — is a replay and answers
`409`, so a sender that wants a second run sends a new timestamp (the guard is
in memory; a restart inside the five-minute window is the one gap). Only a
delivery that FIRED is remembered: one refused before anything started — the
session busy or at its cap, the server draining — may be resent as it was.
`POST /triggers/:id/fire` is a person's fire, answering `201` with the task (a
workflow) or `{run_id}` (an agent turn), `400` when its target cannot start,
`409` when the trigger is disabled or the session is busy or at its cap.

### Guardrails — `/api/v1/guardrails`

A guardrail's **name is its identity** — an agent config references it by name
and nothing else, so names are unique across all definitions, and a name that
no longer resolves fails the agent build rather than silently skipping
([invariant 13](../explanation/workbench-invariants.md)). Built-in:
`content_filter` (input + tool_input, regex — jailbreak keywords),
`max_input_length` (input, 50k chars), `max_output_length` (output, 50k chars).

### Memories — `/api/v1/memories`

A memory is one text under a **scope**: `global` (every agent reads it with
every request), `agent` (that agent reads it), or `session` (the model's own
working notes for one session, never injected, read back on demand and
carried into a context reset). `POST` is an upsert by scope and key; `PUT`
changes content and metadata only, the identity must match. Who writes:
[Authorization](#authorization) says who, and the model's part is the same
table ([invariant 64](../explanation/workbench-invariants.md)). `GET /memories`
lists the global and agent scopes (`scope_kind` and `scope_id` narrow);
session memory is read under `/sessions/:id/memory`. Deleting an agent
deletes its memory with it; a row whose agent is already gone is listed to
the admin, who alone may delete it, and edited by nobody. `written_by` says
whether a person or the model wrote a row: the model writes session memory
freely and agent memory only through an approved `memory_write`.

### Sandboxes — `/api/v1/sandboxes`

A **sandbox** is one row: WHERE it runs and WHAT runs on it. A project names
one (decisions §5.36). Two types: `docker` and `e2b`. Every returned row
carries `supports` — the type's capability flags, derived and read-only — and
clients offer actions from those flags, never from the `type` string
([invariant 53](../explanation/workbench-invariants.md)). The top-level
`prompt` (both types) is appended as a SUFFIX to the instructions of every
agent in a session bound to a project on this sandbox — after the agent's own
instructions and its memories, before the skills index; no project means no
sandbox tools and no prompt (decisions §5.33). It is not a credential — it
round-trips unmasked.

`config` is the type's own object (`DockerConfig` / `E2BConfig`). `e2b` is any
service speaking the E2B API (decisions §5.34); the workbench builds no
templates, so `template_id` must already exist there, and every sandbox is
created `secure` — its daemon requires the per-sandbox token, since the
sandbox id is in the public hostname of every port it serves. `headers` are
sent with every request to the service and its sandboxes, for one that
authenticates with its own header rather than the API key; a name the client
sets itself (`X-API-Key`, `X-Access-Token`) is refused. For `docker`,
`host` picks the daemon: empty for this machine's, `ssh://user@host` for a
remote daemon over pure-Go SSH (sshd with streamlocal forwarding and socket
access for the SSH user; no remote docker CLI — decisions §5.27),
`tcp://host:port` for a TCP-exposed one; `ssh_password` comes back masked,
the key file path, known-hosts and agent flag in the clear.
Blank `memory_mb` / `cpus` take a capped workbench default, never "unlimited"
(decisions §5.38). The daemon is the server's one external dependency
([deploying](../howto/workbench-deploy.md#requirements)); a project's storage
is a volume on it (decisions §5.33).

`POST /sandboxes/{id}/test` tells two failures apart: `200 {ok: false}` is a
service that was reached and a command that RAN and failed (non-zero exit or
timeout); a daemon or service that could not be reached at all — a dial or
credential failure — is `502`.

`DELETE` refuses (`409`) while any project lives on the sandbox: a project's
working tree is at that address, and deleting a project is what reclaims it.
For the same reason a `PUT` on a referenced sandbox refuses its identity
fields — `type` and the destination, plus for `e2b` the three a resume cannot
re-apply — answering `409` and counting the projects that block it; every
other field edits freely
([invariant 45](../explanation/workbench-invariants.md),
[decisions §5.36](../explanation/decisions.md)). Editing the image reaches
bound sessions at their next run and replaces their containers; an edit
outside what a container is made of (a read cap, an SSH setting) hands the
running container to the next generation instead (decisions §5.66); editing
the `prompt` is not a content change at all — it retires no instance and
severs no terminal, the same as a rename.

A `PUT` on a sandbox or a project MAY carry the `revision` the edit was based
on (from GET/List) to make the write a compare-and-set — a concurrent update
is then `409`, re-read and retry; omitting it is last-writer-wins, anchored on
the row's current revision, which stops an in-handler race but not a
stale-client overwrite. Create and update validate the config STRICTLY and
store it in canonical form — unknown keys dropped, fields re-marshalled in
struct order; a type mismatch on a known field or a malformed host is `400`.
One decoder answers every question about a config (save-time validation, the
content comparison, the identity freeze), so they cannot disagree.

The `containers` routes — list, stop, remove by name — are the operator's
reclaim surface on a daemon, and DOCKER only: a sandbox of another type is
refused by name (`400`).

Every sandbox can host a web terminal and `exec_command`'s persistent shells
(spec §2.7k): named shells are scoped to one run, and its teardown closes them
— an approval pause included, so a resumed run reopens its sessions fresh.
Tool output toward the model is capped above the SDK defaults: file reads at
64 KiB (whole source files), exec output at 32 KiB per stream (truncation
keeps head and tail).

### Projects — `/api/v1/projects`

A project is one user's working tree on one sandbox — the unit a session binds
and the unit a container mounts at `/workspace`; the volume and container
naming and the per-project container lifecycle are
[decisions §5.28](../explanation/decisions.md) and §5.33. `sandbox_id` may
change, but only to a sandbox at the SAME destination — how a project changes
its image, and no further (`409`; decisions §5.36). On an `e2b` sandbox the
storage IS the instance, and the server records which one before the client
first uses it — a sandbox nobody recorded is billed compute nobody will ever
stop; the handle is not on the wire. Projects are **personal**: a member manages
their own, an admin additionally manages the plane (`?all=true`, delete, stop,
rebuild; never the export or the environment) — [Authorization](#authorization).

`DELETE` refuses (`409`) while any session binds the project, and otherwise
**destroys the working tree**: the container and its volume are removed
(decisions §5.33) — export what matters first. The row is gone whenever it
answers, and a `storage_error` names storage left for the operator, NOT a
project that survived
([invariant 46](../explanation/workbench-invariants.md)).

An unreferenced container is **idle-stopped** after `sandbox_idle_minutes`
with no run or terminal using it: stopped, not removed, so installed packages
survive and the next run starts it again. The same acts are available by hand
under `/projects/{id}/sandbox` (`absent` / `stopped` / `running`, `start`,
`stop`, `rebuild`): a `start` is where the image pull happens, with a person
watching, rather than inside the next run; a `stop` answered `stopped: false`
dooms the instance — nothing new joins it, and it stops when the run or
terminal using it ends, since the person asked for the sandbox to stop, not
for the work to die; a `rebuild` on an E2B-compatible target is refused with
the way out (decisions §5.34).

A project carries the **environment** its container is created with, so
`exec_command`, a persistent shell and a terminal all read the same values.
Values are write-only, like every other credential here
([decisions §5.32](../explanation/decisions.md)): `GET /projects/{id}` is the
ONE endpoint that returns an environment, as names with masked values and only
to the project's owner — listings never carry it, and an admin's management
reach does not extend to reading one. Changing an environment replaces the
container **at the project's next run** and severs that project's terminals,
its siblings on the same sandbox untouched; files under `/workspace` survive,
anything installed into the container does not, and a rename does neither.

`GET /projects/{id}/export` is how files leave a sandbox whose storage the
host cannot open directly. Its headers go out before the first byte, so a
failure mid-stream cannot become a JSON error — the client sees a truncated
archive, which tar itself reports.

`GET /projects/{id}/host` names where a port inside the sandbox is public:
the sandbox id and the domain the service returned, from which a client
renders `https://<port>-<sandbox_id>.<domain>` (decisions §5.70). Only a
sandbox whose row declares `supports.public_host` answers; a project whose
sandbox was never provisioned is `409`. It reads — it neither creates nor
resumes the sandbox.

### Attachments — `/api/v1/attachments`

Image input for a session's messages ([attachments](../howto/attachments.md);
workbench invariants 56–58). An upload is validated by DECODING the image,
never by its declared content type, and stored under
`attachments/<owner id>/<uuid>.<ext>`; a foreign id reads as absent (`404`),
and once a run accepted an upload it is part of session history (`DELETE` is
`409`). The storage section is written as ONE value, admin-only, probed end to
end before it is stored, which is why `PUT /settings/:key` refuses the `s3_*`
keys one at a time; the form is
[attachments](../howto/attachments.md#configuring-the-bucket).

### Playground — `/api/v1/playground`

`POST /playground/generate` is the trace panel's Replay: one model call from
a stored generation span with edits, `agent_config_id` choosing whose provider
and default model answer, `tools` schema-only definitions (echoed from the
traced request, or picked from `GET /agents/:id/tools`) so the model can emit
calls that nothing executes. `400` for an agent that cannot be built or a
model that cannot be resolved, `502` when the model call fails; the body cap
is 64 MiB, since a replay posts a whole span payload back.

### ChatGPT OAuth

Login, complete, and logout are per-provider, under the provider resource —
see [Providers](#providers--apiv1providers); whether a provider is signed in
is the list's derived `chatgpt_logged_in`, not a separate status call. The
flow is a pasted callback URL, not a loopback listener: `login` answers an
authorize URL, and the URL the browser lands on afterwards goes into
`complete`
([decisions §5.41](../explanation/decisions.md#541-chatgpt-login-redeems-a-pasted-callback-url-not-a-loopback-listener)).

A `chatgpt_login` provider talks to the Codex backend
(`chatgpt.com/backend-api/codex`), which differs from the standard API in two
ways the bridge absorbs: request bodies are rewritten (`store: false`, no
`previous_response_id`, input sanitized, and a reasoning item dropped unless
it carries a codex-native `encrypted_content`), and **only streaming requests
are accepted** — blocking callers such as title generation, compaction
summaries and the playground are served by an internal stream
(decisions §5.15).

### Secret handling

Secret fields are **write-only**: GET responses mask them as `********` and
the plaintext is never sent to a client. On write, the mask keeps the stored
value, a new value replaces it, `""` clears it — so the UI round-trips whole
objects without ever seeing a plaintext. Masked fields: provider `api_key`,
MCP `headers` values and `oauth_client_secret`, the sandbox `ssh_password`,
e2b `api_key` and `headers` values, a project's environment values, and the settings the
registry marks secret. A model-API key crosses exactly one surface — the
provider; an agent carries none (its fallback entries name providers).

**A masked key round-trips only to the destination it was stored for.**
Changing a provider's `type` OR `base_url` while keeping the `********` mask
is rejected with `400` (replace the key or clear it) — restoring it would send
the previous backend's real credential to another endpoint.

**At rest, secrets are sealed under one process key.** Set `AGENTS_SECRET_KEY`
(or `--secret-key-file`) to a 32-byte key — `openssl rand -base64 32` — and
every credential column (the masked fields above, the ChatGPT token, MCP
`oauth_token`, webhook `secret`) is stored AES-256-GCM encrypted
(`enc:v2:<key id>:…`, the key id being the first bytes of the key's SHA-256),
so possession of the database is not possession of every upstream credential.
Each value is bound to its place — `table.column`, plus the field for a
credential inside a JSON column, or the project id for an environment value —
as the cipher's additional data, so a ciphertext moved to another column or
project does not open there; a value pasted in through the API that already
looks sealed is sealed again as the text it is. Without a key the server logs
one warning and stores plaintext — the single-user workbench. Rows written
before a key was set stay plaintext until their next write, and open either
way; a sealed row with no key, or under another key, is a loud error naming
the key ids, never ciphertext handed out as a credential. The first start with
a key seals a canary (`settings.secret_key_check`) and every start after opens
it, so a missing or wrong key refuses to start with one message. There is no
rotation: losing the key loses the secrets; the recovery is the key itself or
a fresh database.

### Health

`GET /health` (outside `/api/v1`, unauthenticated) answers `{status: "ok",
version}` — the liveness probe for containers and load balancers.

### OpenAPI

A generated OpenAPI 3.1 document (YAML) is served at `GET /api/v1/openapi.yaml`
(unauthenticated); the frontend's request/response types are generated from
it in turn, and CI fails when either is stale. There is intentionally no
bundled Swagger/Redoc UI — import the YAML into your own tool.

---

## WebSocket protocol

Endpoint: `GET /ws`. No token in the query string: after connecting the client
sends `{"type":"auth","token":"..."}` as its first message and the server
replies `{"type":"auth.ok"}`. Every message is `{"type":…, "payload":{…}}`.

An inbound frame over 1 MiB closes the socket with `1009`, and no `run.error`
can follow a close frame — so the composer refuses a prompt past that size
before sending it. After `auth.ok` the server pings every 25 seconds and drops
a connection that answers no ping for 60, so a half-open connection does not
pin its goroutine and buffers until TCP keepalive notices; browsers and
standard WebSocket libraries answer pings automatically, and a custom client
only needs to keep reading. A pong counts only while the server is reading, so
a handler about to not read for a while (the terminal endpoint dialing a host
or pulling an image) lifts the deadline for that stretch.

Run events are a **broadcast bus, per owner** — every connection of a session
owner's hears each of that owner's runs, however started, and nobody else's
([invariant 14](../explanation/workbench-invariants.md)). A dropped socket
does not cancel a run; after reconnecting the server re-attaches the
connection, and `run.subscribe` resumes from a cursor (`from_seq`) without a
full replay. The replay ring holds a run's last 512 events, with its
`run.started` pinned outside the ring so a late subscriber is always told
which run this is.

### Client → Server

| type            | Description                                                                                                     |
|-----------------|-----------------------------------------------------------------------------------------------------------------|
| `run.create`    | Start a run — `{session_id, input, attachment_ids?, agent_config_id?, project_id?, plan?}` (the project matters only until the session's first project-carrying run binds it; `plan` and `attachment_ids` as in the REST body) |
| `run.subscribe` | (Re)attach to a run's event stream — `{run_id, from_seq?}` (omit `from_seq` or `0` replays everything retained) |
| `run.cancel`    | Cancel an in-flight run — `{run_id, mode?}`; `mode: "graceful"` finishes the current turn, default aborts; a run paused for approval is abandoned either way (see [Approvals](#approvals--apiv1approvals)) |
| `run.inject`    | Inject input into the live run — `{run_id, queue, input}`; `queue: "steer"` changes course inside the current exchange, `"next_turn"` is consumed at the next turn boundary, `"follow_up"` starts a new exchange once this one finishes |
| `tool.approve`  | Approve a pending tool call — `{tool_call_id, scope?}`; `scope` widens an `exec_command` approval's trust: `"once"` (default), `"same"` (this exact command, for the session) or `"all"` (every command) |
| `tool.reject`   | Reject a tool call — `{tool_call_id, reason?}`                                                                  |

### Server → Client

| type                    | Description                                                                                                                                             |
|-------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `run.started`           | Run begun — `{run_id, session_id, input, attachments?}`; `input` is the user prompt, so a browser that didn't send it can render the user bubble, and `attachments` its image refs (`{id, url}`) for the same reason. A background task run additionally carries `{task_id, parent_session_id, parent_run_id, tool_call_id, label, kind, attempt, max_attempts}` — `kind` is `workflow` for an execution's step run (a step ending is not a workflow ending), `attempt` is which run of the task this is (1, more after a retry — how a new attempt is told from a replay) and `max_attempts` its ceiling; clients key task state by the durable `task_id`, route events by `run_id`, and send it to the parent session's task list, never a chat timeline |
| `run.agent_start`       | Agent taking its turn — `{run_id, agent_name, agent_config_id?}`; the id names the config behind the agent                                              |
| `run.step`              | Streaming text delta — `{run_id, delta}`                                                                                                                |
| `run.reasoning`         | Streaming reasoning delta — `{run_id, delta}`                                                                                                           |
| `run.message`           | One completed assistant message: a turn's full text, interim narration or final answer, authoritative over its `run.step` deltas — `{run_id, text, item_id?}`; `item_id` is the model item's stable id, what a client dedups a hub replay by (text equality when absent) |
| `run.reasoning_item`    | One completed reasoning block: a turn's full thinking text, authoritative over its `run.reasoning` deltas — `{run_id, text, item_id?}`, deduped like `run.message` |
| `run.tool_call`         | Tool invoked — `{run_id, tool_call_id, tool_name, arguments, needs_approval}`                                                                           |
| `run.tool_progress`     | Partial output from a running tool — `{run_id, call_id, tool_name, delta, renderer?}`; `delta` appends to what the client holds for the call, `renderer` is a display hint (e.g. `terminal`) |
| `run.tool_result`       | Tool output — `{run_id, tool_call_id, output, title?, summary?, renderer?, is_error?, extra?}`; the optional display fields mirror the stored output entry's `display` (`extra` is the tool's `Details` bag), so the live card carries the same data a reload rebuilds. A multimodal result's `output` is the Responses content list as JSON (`[{"type":"input_text",…},{"type":"input_image","image_url":…},{"type":"input_file",…}]`, SDK spec §2.7b) — the card shows the image and offers the file; anything else is text |
| `run.handoff`           | Agent handoff — `{run_id, from, to, from_id?, to_id?}`; the ids name the config rows behind the agents, for their avatars                               |
| `run.compaction`        | Session compaction running at end of turn — `{run_id, phase: started\|finished, detail?}`                                                               |
| `run.output`            | Final output — `{run_id, final_output}`                                                                                                                 |
| `run.interrupted`       | Paused for tool approval — `{run_id}`; NOT final: the decision resumes the SAME run id, and its events continue the sequence on the same subscription. Sent only once the pause is durable (the `pending_approvals` row written) — a pause that cannot be recorded ends the run as `run.error` (`persist_error`) instead, so nothing is ever announced as awaiting a decision nobody can make |
| `run.diagnostic`        | Trouble the run survived — `{run_id, type, code?, message?, details?}`; `type` is an open vocabulary (`model_retry`, `model_fallback`, `tool_panic`, …), so show unknown kinds generically |
| `run.gap`               | This connection fell behind and events were dropped — `{run_id, dropped, last_good, next}`; resubscribe from `last_good` to refetch. A gap with `last_good: 0` is the ring having moved past the run's start before this connection attached: nothing to refetch (the UI does not ask) |
| `run.error`             | Error — `{run_id?, session_id?, code, message, guardrail?, stage?}`; `session_id` is set when the failure precedes `run.started` (e.g. `session_busy`, `session_not_found`); `guardrail`/`stage` are set when `code` is `guardrail_tripwire` |
| `run.cancelled`         | Cancelled — `{run_id, reason}`; `reason` is `stopped` (a cancel, a task stop) or `superseded` (a newer message on the session while the run waited for approval — [invariant 19](../explanation/workbench-invariants.md)) |
| `session.title_updated` | Title changed — `{session_id, title}`                                                                                                                   |
| `task.updated`          | A background task moved — the task row (`task_id`, `status`, `kind`, `state`, `attempt`, `dismissed`, a paused one's `pending_call_id`…) as the store has it; on the task's run stream when the hub holds that run, else broadcast to every connection |
| `session.project_bound` | The session's first project-carrying run permanently bound its project — `{session_id, project_id}`; published exactly once, by the run that won the bind |
| `trace.span`            | Trace span — `{run_id, trace_id, span_id, error?, data?, payload_omitted?, attachments?, ...}`; `payload_omitted` says the 256KB live cap replaced the payload fields, which the stored row still has; `attachments` lists the image attachments the span's input references, resolved to `{id, url}` as `run.started` carries the message's |

Generation spans carry the full model request/response in their `data` — what
each call sent after compaction and filters, MCP and skill tool definitions
included. Stored, those payload elements are content-addressed per session
([invariant 62](../explanation/workbench-invariants.md), decisions §5.50), so
the trace panel opens with the SUMMARY listing (`?summary=true`: rows without
the payload, marked `payload_omitted`) and fetches one span whole
(`GET /sessions/:id/traces/:span_id`, its payload rebuilt into `data`) when it
is opened. A span whose input references image attachments lists them beside
the payload as `attachments: [{id, url}]`, resolved against the current public
base; the items keep the stored reference
([invariant 70](../explanation/workbench-invariants.md)). An element past
`trace_span_data_kb` is replaced with a marker
string, its siblings kept; an element whose blob was pruned reads as
`[omitted: the stored payload was pruned]`. Whether session content is
recorded at all is `trace_include_sensitive_data`
([configuration](configuration.md#runtime-settings)).

### Run error codes

`run.error.code` (and the `code` of a `run.diagnostic`) is one flat
vocabulary: the SDK's `ErrorCode` (`agents.CodeOf`), plus the codes the
workbench adds for failures before or outside a run. The two sets never
collide, and the set grows without a client release — a code a client does
not know renders as a generic error.

| Code | From | Meaning |
|---|---|---|
| `session_busy` | workbench | The session already has a live run (precedes `run.started`, carries `session_id`) |
| `session_not_found` | workbench | No such session, or not the caller's (precedes `run.started`) |
| `run_not_found` | workbench | A `run.subscribe`, `run.cancel` or `run.inject` named a run the hub does not hold |
| `approval_failed` | workbench | A `tool.approve` / `tool.reject` could not resume the run |
| `config_error` | workbench | The agent could not be built from its configuration (a missing provider, an unresolvable guardrail, a `vision` mismatch) |
| `persist_error` | workbench | The turn or the pause could not be written to the session |
| `stream_error` | workbench | A fresh run's segment failed before the SDK classified the error |
| `resume_error` | workbench | A resumed run's segment failed before the SDK classified the error |
| `guardrail_tripwire` | SDK | A guardrail tripped; `guardrail` and `stage` name it |
| `max_turns_exceeded` | SDK | The run hit its turn limit |
| `model_behavior` | SDK | The model produced something the loop cannot act on (a malformed call, an unknown tool) |
| `model_refusal` | SDK | The model refused to answer |
| `user_error` | SDK | A configuration or usage error the caller made |
| `tool_timeout` | SDK | A tool ran past its timeout |
| `tool_loop` | SDK | A tool was called in a loop past the SDK's bound |
| `tool_panic` | SDK | A tool panicked |
| `sandbox_exec` | SDK | A sandbox command failed as infrastructure (daemon down, image missing), not as a non-zero exit |
| `mcp` | SDK | An MCP server call failed |
| `unknown` | SDK | Anything else |

### Terminal endpoint — `GET /ws/terminal`

One interactive sandbox terminal per connection, deliberately separate from
the `/ws` event bus: an ordered byte stream with backpressure, not broadcast
envelopes with replay. Authentication is the same first-message `auth`
handshake, after which the client sends exactly one control envelope:

| type              | Direction | Description                                                                  |
|-------------------|-----------|------------------------------------------------------------------------------|
| `terminal.open`   | C → S     | Start the session — `{project_id, cols?, rows?}`; must be the first message  |
| `terminal.ready`  | S → C     | Shell is live; binary frames flow from here                                  |
| `terminal.error`  | S → C     | Open failed (unknown sandbox or project, foreign project, backend error) |
| `terminal.resize` | C → S     | PTY resize — `{cols, rows}`                                                  |
| `terminal.exit`   | S → C     | Shell exited — `{code}` (`-1` when unknown); the server then closes          |

**Binary WebSocket frames carry the terminal byte stream in both directions**
(client → stdin, PTY output → client); text frames are reserved for the JSON
control envelopes above. The shell opens in exactly the (sandbox, project)
container the session's runs use (`/workspace` mounts the project's tree); a
member reaches their OWN projects (a foreign project reads as absent), an
admin any, and a project on a different sandbox is refused (decisions §5.28).
Terminals are capped per the `max_terminals_per_sandbox` setting, and
updating or deleting a sandbox closes its live terminals.

---

## Open changes

What the protocol is still being moved toward, agreed up front so the
frontend and the backend change together instead of renegotiating a payload
shape three times; changing an item here is a decision, made once. What does
not change: the `{"type", "payload"}` envelope, the first-message `auth`, run
events as a broadcast bus per owner that a dropped socket does not cancel
([invariant 14](../explanation/workbench-invariants.md)), and event types
single-sourced in Go and mirrored in TypeScript
([invariant 15](../explanation/workbench-invariants.md)).

### Every streaming delta carries an entry id

`run.step` and `run.reasoning` are `{run_id, delta}`: nothing says which item
a delta belongs to, so the client attributes deltas by position and
reconciles the guess against what was persisted (`mergeLiveTail` in
`streamReducer.ts`). Frozen: they become one `run.delta` —

```jsonc
{ "run_id": "…", "entry_id": "msg_abc", "field": "text" | "reasoning", "delta": "partial cont" }
```

— appended into a provisional buffer keyed by `entry_id` that the completed
entry replaces wholesale, never merges into; an entry with no preceding delta
renders at once, a delta for an entry already completed is discarded.
Bandwidth stays O(n), and the live view and the persisted view cannot
disagree. `run.tool_progress` becomes a `run.delta` on the tool call's entry.

### One `run.entry` event replaces six item events

`run.message`, `run.reasoning_item`, `run.tool_call`, `run.tool_result`,
`run.handoff` and `run.compaction` each carry an item in a shape of their
own; the SDK's `session.Entry` makes them one thing. Frozen: `run.entry`
carries one completed entry — `{id, seq, kind, parent_id, source, display,
usage, payload}` — **byte-identical in shape** to what
`/sessions/:id/messages` returns, so one renderer serves both and the
live-tail merge goes away. `kind` is an open vocabulary, an unknown value of
which renders through the `custom` fallback; `display` stays a rendering hint
a client may ignore and still build a correct timeline from `payload`. The
lifecycle events (`run.started`, `run.agent_start`, `run.output`,
`run.error`, `run.interrupted`, `run.cancelled`) stay; `run.compaction`
survives as the progress signal while the checkpoint itself arrives as a
`run.entry` of `kind: "compaction"`. The two changes travel together —
roughly half the streaming reducer becomes a replace-by-id; the
reconciliation between a history fetch and the live events that arrived
while it was in flight is a client ordering problem no payload shape touches.
`run.message.item_id` is the first piece of this already on the wire.

### Known gaps

- An expired **chat** approval leaves the hub's record `interrupted` until
  the run's retention passes; only a task approval's expiry publishes a
  terminal `run.cancelled`. `GET /runs/:id` inside that window shows a pause
  no decision can resume.
- `run.tool_progress` can arrive twice across a gap replay: the envelope
  carries no sequence number, so a replayed delta cannot be told from a fresh
  one until the envelope carries `seq`.
