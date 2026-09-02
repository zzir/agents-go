# The workbench's wire surface

What `agents-server` exposes over HTTP and WebSocket, and what each call
*means* — the semantics a schema cannot state.

**Endpoint and payload schemas are not repeated here.** They are generated from
the handlers into the OpenAPI document, which CI diffs on every change; a
running server serves it at `GET /api/v1/openapi.yaml` (unauthenticated — see
[OpenAPI](#openapi)). This page carries what that document cannot: which calls
are refused and why, what a write does to rows it does not name, and how two
endpoints compose.

The two WebSocket changes still open — entry ids on streaming deltas, one
`run.entry` event — are frozen in a document that lives next to the code it
governs: [`cmd/agents-server/PROTOCOL.md`](../../cmd/agents-server/PROTOCOL.md).

---

## REST API

Base path `/api/v1` — the only mount; there is no unversioned alias. All
request and response bodies are JSON. Request bodies are capped at 1 MiB
(matching the WebSocket frame limit): a declared length past it answers
`413`, an undeclared body is cut there and fails its decode. Two routes
carry more: `POST /playground/generate` replays a stored span payload, so
its cap is 64 MiB, and
`POST /attachments` takes an image, so its cap is the 10 MiB image limit
plus multipart slack.

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
| `forbidden`    | 403  | Refused for who the caller is: admin role required, the row belongs to another user, or a member writing admin-only configuration |
| `not_found`    | 404  | No such resource                                                              |
| `conflict`     | 409  | Resource is in the wrong state for the request                                |
| `upstream`     | 502  | A failing upstream dependency (model provider, MCP server, sandbox host, git) |
| `internal`     | 500  | Unexpected server error (detail is logged, not returned)                      |
| `unavailable`  | 503  | Transient refusal while the server drains for shutdown — retry later          |
| `rate_limited` | 429  | This client IP exceeded an endpoint's rate budget — slow down and retry       |

### Response conventions

- **Create** returns `201 Created` with the created resource.
- **Update** (`PUT`/`PATCH`) returns `200 OK` with the full updated resource —
  except `PATCH /auth/users/:id`, which answers `204` with no body.
- **Delete** returns `204 No Content`.
- A write (update or delete) against a missing resource returns `404`.
- Secret fields are write-only — see [Secret handling](#secret-handling).

### Sessions — `/api/v1/sessions`

`POST /sessions` accepts an optional `agent_config_id` to bind the session to an
agent at creation. It must reference an agent VISIBLE to the caller — a foreign
private id reads as absent and answers `400`. Rename and pin are a single
`PATCH /sessions/:id` accepting a partial `{name?, pinned?}` body.

Session responses also carry `project_id?` — the session's project binding,
written by its first project-carrying run (see [Runs](#runs--apiv1runs));
absent while unbound. The binding is **immutable**: it cannot be set or
changed over the API — one conversation, one file system context. Switching
projects means starting (or forking into) another session; the composer's
Project picker is that flow in the UI.

`fork` copies the source session's entries (and their traces) into a new
session. Its body is optional: `{message_id?, exclusive?, label?}`. Omit
`message_id` to fork everything; supply it to bound the copy up to and including
that entry (`exclusive: true` excludes the boundary entry itself). Entry ids and
their parent links are rewritten into the fork's namespace, so the copy is a
self-consistent tree rather than one pointing back at another session. The
session inherits the source's `agent_config_id` and its `project_id` — a fork
continues the same conversation over the same file system context, with no
fresh bind of its own.

`branch` moves the session's active branch to an entry, so the next run
continues from there. It APPENDS a leaf entry rather than deleting anything:
the abandoned attempt stays recorded, which is what makes "regenerate" show a
"2 / 3 ‹ ›" switcher instead of filling the session list with `(regen 2)`,
`(regen 3)` copies. Each entry reports `on_path` — false means an abandoned
attempt, still stored and still switchable-to.

Regenerating is `branch` back to the user's message followed by a run with an
EMPTY input: nothing to add, history to answer.

`/sessions/:id/messages` returns **session entries** — the SDK's
`session.Entry` as the runner wrote it, plus the row id the cursor pages
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
ends). The fields: `input_tokens`, the LAST model call's input (not
`session_input_tokens`, which totals every call and so counts re-sent history
once per turn); `context_window`, the agent config's declared window
(`provider.context_window`; 0 when unset, and the panel then shows occupancy
without a denominator); `conversation_tokens`, the transcript still in context
(every active, uncompacted entry summed); and `compaction_tokens`. Compacted
and off-path entries keep their usage — the call happened — but leave the last
two, because the model no longer sees them. Which ruler each figure is on — a
provider count or a character estimate — and why they never mix is
[invariant 28](../explanation/workbench-invariants.md).

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
`?before_id=`. Without `limit` the full list is returned (oldest-first).
With `limit`, the newest `limit` items
are returned; page backwards by passing the smallest id you received as
`before_id` (an exclusive upper bound). Row ids are UUIDv7 strings and order
by insertion — `NewV7` is monotonic within a process — so "smallest" is the
first one in a page. For `messages` the limit counts the
ENTRIES a client receives, not table rows — update entries are folded into
their targets first, so a page is never short of what was asked for.

### Runs — `/api/v1/runs`

Runs are the REST surface for starting and observing agent executions. They share
the same run hub as the WebSocket transport, so a run started over either
transport is observable over both. Crucially, **runs execute server-side,
independent of the connection that started them** — a dropped client or a page
reload does NOT cancel the run. Reconnect and resubscribe (via
`GET /runs/:id/events` or the WebSocket `run.subscribe`) to pick the stream back
up without loss.

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
session already has an active run. `plan` (a bool) asks the session to enter
(`true`) or leave (`false`) the planning phase with this run; absent leaves
the phase as it stands ([invariant 33](../explanation/workbench-invariants.md)).

The first run that carries a `project_id` **permanently binds** it to the
session — validated first (the project must exist and be the caller's own; a
request that fails validation is `400` and leaves the session unbound),
announced once with a `session.project_bound` event. A run naming no project
never binds, and gets **no sandbox tools at all**: no project, no working
tree, no file or command tools (decisions §5.33). From then on the server uses
the bound value and ignores whatever the client sends —
[invariant 27](../explanation/workbench-invariants.md) holds the full contract.

`GET /runs/:id` returns `{run_id, session_id, status, last_seq, agent_config_id?,
project_id?, task?}` — `task` is present only for a background
task's run, carrying the parent linkage the matching `run.started` event does.
`status` is one of `running`, `interrupted`, `completed`, `error`,
or `cancelled`. Finished runs stay queryable and replayable for **15 minutes**
after they end, then `GET /runs/:id` returns 404 (the conversation itself is
always in `/sessions/:id/messages`).

`GET /runs/:id/events` is a Server-Sent Events stream. (This is plain HTTP SSE
for API consumers — unrelated to MCP's deprecated SSE transport, which this
server does not expose.) Each event's `id:` is the hub sequence number;
reconnect with the `Last-Event-ID` header (or `?from_seq=`) to resume without
losing events. The stream closes after a FINAL event — `run.output`,
`run.error` or `run.cancelled` — and when the hub tears the run's subscription
down (its retention window passing, a shutdown), so a stream never outlives
the run it follows. `run.interrupted` (paused for approval) does
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

Unanswered approvals expire — configurable via the `approval_ttl_minutes`
setting. On timeout the pending record is dropped and an error annotation is
written to the session so the timeout is visible rather than silently
vanishing.

### Tasks — `/api/v1/tasks`

A task is one piece of background work started from a chat through the ONE
tool that starts any: `spawn_task` — a sub-agent on a prompt, or, told a
`workflow` name, a workflow execution (`kind: "workflow"`, see
[Workflows](#workflows--apiv1workflows)).
Each runs on its own hidden session and reports back by injecting a
notification into the parent session (see the SDK's
[background tasks](../howto/tasks.md)).

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
live-task cap — the `max_tasks_per_session` setting, which a retry queues
behind like a spawn. `dismiss` hides a finished row from the chat strip (the
panel still lists it): `409` while it is still running, and a retry brings it
back.

`GET /sessions/:id/tasks` lists one session's tasks, newest first. `GET /tasks`
pages every live session's, each row with its conversation's name, plus a
`total` for the pager: `?kind=workflow` narrows to executions, `?live=true` to
`working` / `input_required` rows, `?limit=` (500 at most) and `?offset=` page
it.

### Agents — `/api/v1/agents`

Agent config shape — the top-level scalars, then the knobs as **grouped
nested objects** (each group is one JSON column in the table, so a new knob
needs no schema change), then a few top-level JSON blobs:

- **Top level**: `name`, `avatar` (the agent's picture as a path into the
  UI's built-in catalog, `/avatars/<name>.svg`; empty renders an initial, and
  anything else — an external URL included — is rejected with 400, since the
  CSP would block it anyway), `description` (what the agent is FOR, in a sentence
  — the text an automatic agent picker matches a request against; never sent
  to the model), `instructions`, `model`, `provider_id` (the endpoint
  this agent reaches its model through — see [providers](#providers--apiv1providers);
  empty reaches no credential, so the run fails its pre-flight until the
  agent names a provider), `context_window` (declared, 0 = unknown)
- **`behavior`**: `max_turns`, `handoff_description`, `tool_choice_reset`,
  `stop_at_tools` (comma-separated tool names — the run ends after a turn that
  called any of them), `handoff_input_filter`, `max_tool_concurrency`,
  `tool_not_found_behavior` (unset feeds a tool name the agent does not have
  back to the model so it can correct itself; `error` ends the run instead),
  `reasoning_item_id_policy` (`preserve` / `omit`), `workflow_authoring`
  (gives the agent's chat runs `get_workflow` / `save_workflow` — off by
  default; see [invariant 39](../explanation/workbench-invariants.md))

  Plan and todo mode are NOT here. `todo_write` is on every chat agent — when a
  job is worth tracking is the model's judgement, like any other tool. Plan mode
  is a restraint, so it belongs to the session and the person: it rides on the
  run request (`plan`), and the session reports it as `planning`. Workflow
  authoring IS here, and off by default — see
  [invariant 39](../explanation/workbench-invariants.md).
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

`GET /agents/:id/tools` reports the agent's CURRENT tool surface as schema-only
definitions (`name`, `description`, `parameters`): the built-in tools its runs
carry, connected MCP servers' tools (a server whose listing fails is skipped,
not fatal) and the skills reader — everything but sandbox tools, since no
sandbox is selected. It backs the Replay dialog's tool picker; nothing is
executed from it, and a member sees the surface their own runs would get.

An agent body carries no model-API credential at all — it names a provider,
which is where the key lives (the ChatGPT OAuth flow included — see
[providers](#providers--apiv1providers)); its remaining credential fields come
back masked — see [Secret handling](#secret-handling).

### MCP Servers — `/api/v1/mcp-servers`

The one transport is streamable HTTP (decisions §5.25) — `config` is `{endpoint,
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

The interactive flow logs each step, so a stuck `authorizing` is diagnosable
from the server log — [MCP OAuth troubleshooting](../howto/mcp-oauth-troubleshooting.md)
reads those lines.

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

The secret-bearing config fields come back masked — see
[Secret handling](#secret-handling).

### Memories — `/api/v1/memories`

Memories can be scoped to a specific agent via `agent_config_id`.

### Settings — `/api/v1/settings`

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
registry no longer defines with `"unknown": true` and its value masked
(whether it WAS a secret is unknowable once the def is gone), and `DELETE`
takes it, so a value left behind by an older build can be seen and cleared
rather than being hidden with no way to remove it.

The keys, grouped as the panel shows them, with their defaults, are tabled
once in the [configuration reference](configuration.md#runtime-settings);
`GET /setting-defs` is the live copy. One group is not written key by key: the
`storage` keys (the attachment bucket) save as one section through
`PUT /attachments/storage` — see [Attachments](#attachments--apiv1attachments).
Trace size is a disk budget: a session's payload elements are stored once
(`trace_blobs`), so what every model call adds is a span row and a reference
list as long as the conversation; `trace_retention_days` and
`trace_payload_retention_days` are the other half of that budget.

### Server info — `/api/v1/server` (read-only)

`{version, timezone, credentials_sealed}` — the process facts a client is
subject to but cannot change: the build version; the server's local time zone
(an IANA name — cron triggers tick in it, so a client showing a schedule or a
`next_fire_at` can say which zone it means); and whether a credential-sealing
key is in force (`AGENTS_SECRET_KEY` / `--secret-key-file`, see
[Secret handling](#secret-handling)), so a panel can say that stored secrets
are plaintext instead of leaving it to be discovered. Caps are settings, not
reported here (`max_tasks_per_session` and the rest read through `/settings`).

### Skills — `/api/v1/skills`

A skill is one stored `SKILL.md` document (decisions §5.26): `content` is the
input, `name` and `description` are read from its frontmatter at save time.
The model discovers skills through an index in the agent's instructions and
reads a document on demand with the `read_skill` tool; an agent's `skills`
selection (skill ids) restricts both.

**A skill's identity carries its repository** (decisions §5.31). The model-facing
name is `owner/repo:name` for a GitHub import, `host:name` for another URL,
and the bare frontmatter name for a skill authored in the workbench — so two
repositories may each ship a `review`, and uniqueness is per
`(repo label, name)` within a visibility context rather than per name (the
label is stored as `repo_label`, so two source URLs reducing to one label
collide as they should). A name collision therefore only happens within one
repository — or between two sources sharing a label — and the second file is
skipped with a reason.

**A repository's skills publish as one group.** Scope moves per
`(source_repo, owner)` group, all or nothing; `POST /skills/:id/scope`
serves workbench-authored rows only, and an imported group flips through
`POST /skill-repos/scope` with `{repo, scope, owner_id?}` — the group is named
`(repo, owner)`, defaulting to the caller's own, and `owner_id` is how an admin
promotes a member's import. `404` for a group nobody has, `409` when it is
already in that scope or a name collides in the target scope.

`POST /skill-imports` with `{url}` upserts skills from elsewhere:
`https://github.com/owner/repo` walks the repository via the GitHub API,
anonymously (HEAD commit → full tree → every `SKILL.md` at any depth, all
pinned to one commit; two API calls plus one raw fetch per `SKILL.md` — up to
~202 requests at the 200-skill cap — private repositories are not reachable).
Any other http(s) URL is fetched as a single raw `SKILL.md`. Each fetch is
bounded by a 30-second timeout, and a failed fetch answers `502`. The
response names the `repo` and lists what was
`created` / `updated` / `unchanged` / `skipped` (each skip with its reason);
`truncated` reports that GitHub's tree listing was cut off — files past the
cut were not seen at all.
Re-importing the same source refreshes rows that were not edited locally;
editing an imported skill **detaches** it, and a detached skill is never
overwritten by an import. A sync's NEW files inherit the group's scope and
owner, so a published repository never splits itself on an upstream addition.
Documents are capped at 256 KiB, imports at 200 skills per repository.

### Providers — `/api/v1/providers`

One configured endpoint and the credential that reaches it: `name`, `type`
(`openai` default / `anthropic` — selects the API protocol), `auth_mode`
(`chatgpt_login` is openai-only), `api_key`, `base_url`. Agents REFERENCE a
provider by id; nothing else stores a model-API key, so this
is the one surface a credential crosses (masked on read; a mask round-trips
only to the destination it was stored for — see
[Secret handling](#secret-handling)).

The ChatGPT OAuth flow lives here too, for the same reason: the token is this
endpoint's credential, so every agent pointed at the provider shares one login.
`POST /providers/:id/chatgpt/login` · `/complete` · `/logout` — see
[ChatGPT OAuth](#chatgpt-oauth).

`GET /provider-types` (read-only) lists the registered backends as machine
facts — `type`, `auth_modes`, `unsupported` request features — straight from
the server's provider registry, which is also what validation and provider
construction derive from. The UI's capability hints read this endpoint, so
they cannot drift from what the build enforces; adding a backend is one
registry entry plus a frontend metadata row.

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
launched ([invariant 29](../explanation/workbench-invariants.md)). There is no second execution
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
request against when `spawn_task` lists the workflows on offer, and an agent
naming one is how a workflow usually starts; a person starts one with a brief
of their own — the `POST` start in the table below ([invariant
30](../explanation/workbench-invariants.md) holds the brief contract). A step may set `compact_before`,
folding the conversation into a summary before it runs — with the step's own
agent's compaction settings, since that agent is the one about to read the
summary (an agent whose compaction is off leaves the transcript as it is,
logged): a step boundary is the
natural place for it, since the exploration that got the sequence here is
spent and every later step pays for it otherwise. A step may set
`pause_before`: the sequence holds there until a person approves it from the
conversation that asked ([invariant 37](../explanation/workbench-invariants.md)) — for the deploy
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
a workflow deterministic ([invariant 30](../explanation/workbench-invariants.md)); with `gate` and
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
opted in (`behavior.workflow_authoring` — off by default): `get_workflow(name)`
reads a definition and `save_workflow(...)` creates or updates one — steps,
agents and edges by NAME, never by id (`{name, description, steps: [{name,
agent, prompt, gate, gate_pass, gate_fail, pause_before, compact_before,
on_success, on_failure}], budget}`, edges naming a step or `end`; the save
tool's description lists the agents on offer). Saving under a name that exists
replaces that definition — an update, not a second workflow — and every save
is APPROVED first: the approval card in the chat is the review, the definition
drawn as in the hub and, on an update, the stored definition diffed line by
line. A saved workflow can be started in the same turn —
`spawn_task(workflow=name)` reads the store, not its own listing — and, like
any edit, changes nothing already in flight. The gate, the name-keyed shape
and who may write what are [invariant 39](../explanation/workbench-invariants.md)'s.

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
stops, failed with the reason (`budget exhausted: 4 of 3 steps`), and a
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

A trigger row carries `enabled`, `last_error`, `last_started_id` and — for an
enabled cron trigger — `next_fire_at` (RFC 3339), the next tick as the
scheduler holds it, absent when disabled or for a webhook. Schedules tick in
the server's local zone, which `GET /server` reports as `timezone`. A webhook
row also carries `hook_path` (`/hooks/:id`, outside `/api/v1`) and
`secret_hint` (the secret's last four characters); the `secret` itself rides
only the create response and `POST /triggers/:id/rotate-secret` (the old
secret stops working the moment a rotation answers; `400` on a cron trigger).
`POST /triggers/:id/fire` is a person's fire — as a tick or a webhook call
would, with an optional `{payload}` appended to the brief — answering `201`
with the task (a workflow) or `{run_id}` (an agent turn), `400` when the
trigger is disabled or its target cannot start, `409` when the session is busy
or at its cap. Triggers are capped at 50 per owner (`409` above it).

In the UI the sidebar's **Workflows** button opens the hub — Definitions,
Triggers, Runs — and `/workflow <name> <brief>` in a conversation's composer
starts one into it, the same start `Run…` makes;
[Workflows](../howto/workflows.md) walks both.

Executions are tasks, acted on through the [Tasks API](#tasks--apiv1tasks).
Only a FAILED execution retries — re-running a completed or cancelled one
would repeat its side effects — a restart fails whatever was running, at the
step it reached, for the same reason, and deleting a session stops its tasks
first — executions included — so no step keeps causing side effects after the
row is gone.

### Guardrails — `/api/v1/guardrails`

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

A **sandbox** is one row: WHERE it runs and WHAT runs on it. A project names
one (decisions §5.36). Two types: `docker` and `e2b`.

Every returned row carries `supports` — the type's capability flags, derived
and read-only: `rebuild` (the container can be replaced while the working
tree survives). Clients offer actions from these flags, never from the
`type` string (workbench invariant 53).

A sandbox may also carry a top-level `prompt` (both types) — free text
describing the machine: the toolchain the image ships, whether it has network,
which package manager to reach for. It is appended as a SUFFIX to the
instructions of every agent in a session bound to a project on this sandbox,
after the agent's own instructions and its memories and before the skills
index. No project means no sandbox tools and no prompt (decisions §5.33): a chat
with no working tree never sees it. It is not a credential — it round-trips
unmasked.

For `e2b` — any service speaking the E2B API: E2B's own cloud, a self-hosted
E2B, or a compatible service such as Alibaba Cloud's Function Compute cloud
sandbox (decisions §5.34) — the config is `api_url` (control plane; empty =
E2B's own), `domain` (the suffix a sandbox's public hosts are built from),
`api_key` (write-only, masked), `data_plane_auth` (`""` auto, `access_token`,
`api_key` or `none` — which credential the in-sandbox daemon takes; the
compatible services differ, so it is configuration), `template_id` (required —
the workbench builds no templates; the service's console or CLI does),
`timeout_seconds` (the lease a sandbox is created and refreshed with),
`auto_pause` (an expired lease PAUSES rather than kills — **defaults to true**,
the safe choice for stored working trees; an explicit `false` destroys the tree
on expiry, and some services gate pausing behind a per-function feature and
refuse the create without one), `allow_internet`, and `max_read_file_bytes`.
Every sandbox is created `secure`, so its daemon requires the per-sandbox
token: without that, anyone who learns the sandbox id — which is in the public
hostname of every port it serves — reaches its files (decisions §5.34).

For `docker`, the Docker daemon is the server's ONE external dependency: it shells out to no
binary — not git (skills import over the GitHub API), not ssh (remote
daemons over pure-Go SSH), not the docker CLI (the socket API).
`config.host` picks the daemon: empty for this machine's, `ssh://user@host`
for a remote daemon reached over SSH (the remote needs sshd with
streamlocal forwarding and the SSH user in the docker group, no remote docker
CLI), `tcp://host:port` for a TCP-exposed one. The `ssh_*` fields carry the
SSH authentication and come back masked — see
[Secret handling](#secret-handling). The rest is what runs: `image`
(required), `runtime` (e.g. `runsc` for gVisor — whether it exists is that
machine's business), `user` (user[:group]; empty runs as **root**, so an agent
can install packages into its own container), `network` (the docker network
name to join; empty means no network at all), `memory_mb` / `cpus` caps (blank
`0` takes a safe workbench default — 4096 MiB, 2 CPUs — never "unlimited", since
agent code runs here; raise them per sandbox), and
`max_read_file_bytes` — how large a file the read tool will load at all
(`0` = the 8 MiB default), a guard on the read itself, distinct from the
64 KiB cap below on what the model is SHOWN of it.

Storage is a docker volume on the sandbox's daemon, so the server never
touches a host filesystem (decisions §5.33).
`POST /sandboxes/{id}/test` runs `echo ok` in a throw-away sandbox — a
container for `docker`, a provisioned-and-destroyed instance for a remote
service. `200 {ok:false}` means the service was reached and the command RAN but
did not succeed (non-zero exit or timeout); a daemon or service that could not
be reached at all — a dial or credential failure — is `502`, a different thing
a caller must tell apart.

`DELETE` refuses (`409`) while any project lives on the sandbox: a project's
working tree is at that address, and deleting a project is what reclaims it.
`PUT` freezes a referenced sandbox's **identity fields** — `type` and the
destination (`host`, or `api_url` + `domain`) — for the same reason, counting
the projects that block it. For an e2b sandbox the freeze also covers
`template_id`, `auto_pause` and `allow_internet`: a `/connect` resume
re-attaches to the already-provisioned instance and cannot re-apply them, so an
edit that projects block is `409` rather than a save that silently never takes
effect. `timeout_seconds` is exempt — resume re-sends it — as is `user`, which
rides every command and so lands on the next one. The image, the limits,
the credentials and the name stay freely editable; key rotation is routine, and
an image change replaces the containers at their next run. The `prompt` is
editable too, but unlike the image it is NOT a content change: it retires no
instance and severs no terminal, reaching bound sessions at their next run — the
same as a rename.

A sandbox carries one monotonic counter, `revision`: every write bumps it. A
`PUT` MAY carry the `revision` the edit was based on (from GET/List) to make the
write a compare-and-set — a concurrent update then means `409`, re-read and
retry; omitting it is last-writer-wins, anchored on the row's current revision,
which stops an in-handler race but not a stale-client overwrite. There is no
per-sandbox runtime
generation — the ONE runtime axis is the project's, and a content change here
bumps it on every project that names this sandbox, which is what retires their
live instances and severs their terminals.

Create and update validate the config STRICTLY and store it in canonical
form: a type mismatch on a known field or a malformed host is `400`.
Canonical means unknown keys are dropped and the fields re-marshalled in
struct order. One decoder answers every question about a config — save-time
validation, the content comparison, the identity freeze — so they cannot
disagree.

`GET /sandboxes/{id}/containers` lists this package's containers on the
sandbox's daemon, and the stop/remove routes act on one by name — the
operator's reclaim surface. It is DOCKER only, and a sandbox of another type
is refused by name (`400`).

Every sandbox can host a web terminal and `exec_command`'s **persistent
shells** — every container is persistent. The tool schema offers a
`session_id`, and a named shell is held open between calls so `cd`, exported
variables and an activated environment survive. Named shells are scoped to
one run — its teardown closes them (an approval pause included, so a resumed
run reopens its sessions fresh). Tool output toward the model is capped
above the SDK defaults: file reads at 64 KiB (whole source files), exec
output at 32 KiB per stream (truncation keeps head and tail).

### Projects — `/api/v1/projects`

A project is one user's working tree on one sandbox (decisions §5.28): the
unit a session binds and the unit a container mounts at `/workspace`. The tree
is the named volume `agents-proj-<project id tail>` on the sandbox's daemon — the same on every daemon, local or remote
(decisions §5.33). Containers are one per project, named
`agents-<project tail>`, kept (stopped, not removed) across restarts, with
`/tmp` a RAM-backed tmpfs capped at 1g.

`sandbox_id` may change, but only to a sandbox at the SAME destination — how
a project changes its image, and no further: the files live at that address
and do not travel (`409`). On an `e2b` sandbox the storage IS the instance, so
`instance_ref` remembers which one — recorded before the client will use it,
since a sandbox nobody recorded is billed compute nobody will ever stop.

`DELETE` refuses (`409`) while any session binds the project, and otherwise
**destroys the working tree**: the container and its volume are removed. A
project's storage is what the row was for, and leaving a volume behind on
every delete is an unbounded leak nobody has a listing for
([decisions §5.33](../explanation/decisions.md)) — export what matters first.

Projects are **personal**: every member manages their own; the routes scope
by owner rather than the admin gate. An admin additionally manages the
plane (decisions §5.29's manage-not-author line): `?all=true` lists every
owner's rows — the Settings hub's Projects panel, under Administration — and delete, stop and rebuild
work on any project (each is less than the delete already allowed there);
reading a tree — export — stays the owner's. Listings carry each
row's `session_count`; `storage_hint` (where the files live) is reported to
admins only.

`DELETE /projects/{id}` answers `200 {deleted, storage_error?}`. The row is
gone whenever it answers: a `storage_error` names storage that could not be
reclaimed and is left for the operator, NOT a project that survived — an
error status there would say the opposite.

An unreferenced container is **idle-stopped** — configurable via
`sandbox_idle_minutes` — with no run or terminal using it: stopped, not
removed, so installed packages survive and the next run starts it again. The
same three acts are available by hand: `GET /projects/{id}/sandbox` reports
`absent` / `stopped` / `running` (owner or admin), `POST …/sandbox/start` provisions it (the
image pull happens there, where a person is watching, rather than inside the
next run), and `POST …/sandbox/stop` releases the compute keeping the tree.
A stop while a run or a terminal is still using it answers `stopped: false`:
the instance is doomed so nothing new joins, and it stops when that work ends
— the person asked for the sandbox to stop, not for the work to die.
`POST …/sandbox/rebuild` throws the container away and provisions a fresh one
from the current template, keeping the volume. It is a **docker** operation:
on an E2B-compatible target the sandbox IS the storage, so the rebuild is
refused with the way out — export the project, then create a new one
([decisions §5.34](../explanation/decisions.md)).

A project carries the **environment** its container is created with, so
`exec_command`, a persistent shell and a terminal all read the same values.
Values are **write-only**: sealed at rest and masked in every response, like
every other credential here ([decisions §5.32](../explanation/decisions.md)).
A masked value sent back unchanged keeps what is stored — under the same
name, never a new one — so one variable is rewritten without retyping the
rest. `GET /projects/{id}` is the ONE endpoint that returns an environment,
as names with masked values, and only to the project's owner: listings never
carry it at all, and an admin's management reach does not extend to reading
one.

`GET /projects/{id}/export` streams the working tree as an uncompressed tar
(`application/x-tar`) — the way files leave a sandbox whose storage the host
cannot open directly (decisions §5.33). Owner only, on the same reasoning as
the environment, and **audited**: it takes a whole tree off the machine. The
headers go out before the first byte, so a failure mid-stream cannot become a
JSON error — the client sees a truncated archive, which tar itself reports.

Changing an environment replaces the container **at the project's next run**
(the runtime generation moves, exactly as a template edit does) and
severs that project's terminals — its siblings on the same sandbox are
untouched. Files under `/workspace` survive; anything installed into the
container does not. A rename does neither. An update MAY send the `revision` the
edit was made against to make the write a compare-and-set (a concurrent write
then answers `409`); omitting it is last-writer-wins, anchored on the row's
current revision.

### Attachments — `/api/v1/attachments`

Image input for the chat ([attachments](../howto/attachments.md); workbench
invariants 56–58). `POST /attachments` takes one multipart `file` (png or jpeg,
10 MiB at most, longest side 8000px — validated by decoding, never by the
declared content type), stores the bytes in the configured bucket under
`attachments/<owner id>/<uuid>.<ext>`, and answers `201 {id, url, mime, size}`;
the `id` is what `attachment_ids` names when a run starts. `503` while the
storage section is unset. An upload never sent with a message is
garbage-collected after 24 hours. `DELETE /attachments/:id` is the composer's
✕: owner-only (a foreign id reads as absent, `404`), and one already accepted
by a run is part of session history — `409`. Object first, row second, the
order the reaper uses.

`GET /attachments/config` answers `{enabled, max_bytes, max_count,
downscale_px}` — whether the section is complete and the limits a client
applies before uploading. The storage section itself is written as ONE value,
admin-only: `PUT /attachments/storage` with `{endpoint, region, bucket,
access_key_id, secret_access_key, public_base_url, path_style}` probes the
bucket end to end first (signed upload, anonymous public read through the
public base, delete) and refuses with `400` naming the failing stage, else
stores the seven keys in one transaction and answers the config; an all-empty
body clears the section and turns image input off; a masked
`secret_access_key` keeps the stored secret; an empty `region` is `auto`.
`POST /attachments/storage/test` runs the same probe on the submitted values
without storing them — `204` or `400`.

### Playground — `/api/v1/playground`

`POST /playground/generate` is one model call, made from a stored generation
span with edits — the trace panel's Replay. It touches no session and records
no run. The body is `{agent_config_id, model?, system_instructions?,
input_items, model_settings?, tools?, output_schema?, stream?}`:
`agent_config_id` (required) selects whose provider and default model answer;
`input_items` are Responses input items as edited; `model_settings` replaces
the agent's own when set; `tools` are schema-only definitions echoed from the
traced request (or picked from `GET /agents/:id/tools`) so the model can emit
calls — a single call runs no tool loop and nothing is executed;
`output_schema` (`{name?, schema, strict?}`) replays a structured call as one.
Without `stream` the answer is `{output, usage?, duration_ms, ttft_ms}`, the
`output` being the Responses output items as JSON; with `stream: true` the
response is SSE — `delta` / `reasoning` text events as they arrive, then one
`done` carrying that same object, or `error`. `400` for an agent that cannot be
built or a model that cannot be resolved, `502` when the model call fails. A
replay posts a whole span payload back, so its body cap is 64 MiB, not the
1 MiB default.

### ChatGPT OAuth

Login, complete, and logout are per-provider, under the provider resource — see
[Providers](#providers--apiv1providers). Whether a provider is signed in is read
from the provider list's derived `chatgpt_logged_in` field, not a separate
status call. `login` returns an
authorize URL; the browser redirect after authorizing lands on
`http://localhost:1455/auth/callback`, where nothing on the server listens. The
user copies that URL and submits it to `complete`, which redeems the code
server-side. This replaces the CLI-style loopback listener so a remotely deployed
server — where the browser's `localhost` is not the server's — can still sign in
([decisions §5.41](../explanation/decisions.md#541-chatgpt-login-redeems-a-pasted-callback-url-not-a-loopback-listener)).

A `chatgpt_login` provider talks to the Codex backend
(`chatgpt.com/backend-api/codex`), which differs from the standard API in two
ways the bridge absorbs: request bodies are rewritten (`store: false`, no
`previous_response_id`, input sanitized to the fields the backend accepts, and a
reasoning item dropped unless it carries a codex-native `encrypted_content` — the
backend caps reasoning `content` at length 0 and rejects a blob it did not
produce, so a reasoning trace replayed from another provider is stripped rather
than 400'd),
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
`headers` values and `oauth_client_secret`, and the sandbox `ssh_password`.
A model-API key crosses exactly one surface — the provider — which is what
giving providers their own entity bought.

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
and `headers`, the sandbox `ssh_password`, webhook `secret`, fallback `api_key`s,
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

`GET /health` (outside `/api/v1`, unauthenticated) answers `{status: "ok",
version}` — the liveness probe for containers and load balancers.

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

---

## WebSocket protocol

Endpoint: `GET /ws`

> The two changes still open — entry ids on streaming deltas, one `run.entry`
> event — are frozen in [PROTOCOL.md](../../cmd/agents-server/PROTOCOL.md).
> What follows is what ships today.

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
are a **broadcast bus, per owner**: every connection of a session owner's is
attached to each of that owner's runs — on connect (their in-flight runs, with
a replay of the buffered events) and automatically when one of their runs
starts or resumes, no matter which connection (or REST call) started it —
and nobody else's connection hears a thing
([ownership and roles](../howto/workbench-auth.md#ownership-and-roles)). Two
browsers of the owner's on the same session both watch the conversation live. A dropped socket does not cancel a run; after
reconnecting the server re-attaches the connection, and `run.subscribe` remains
available to resume from a specific cursor (`from_seq`) without a full replay.
The replay ring holds a run's last 512 events; its `run.started` is pinned
outside the ring, so a subscriber from seq 0 is told which run this is even
when the ring has long moved past the start (a browser reloaded a minute into
a run streams live again instead of showing the session idle until it ends).

### Client → Server

| type            | Description                                                                                                     |
|-----------------|-----------------------------------------------------------------------------------------------------------------|
| `run.create`    | Start a run — `{session_id, input, attachment_ids?, agent_config_id?, project_id?, plan?}` (the project matters only until the session's first project-carrying run binds it; `plan` and `attachment_ids` as in the REST body) |
| `run.subscribe` | (Re)attach to a run's event stream — `{run_id, from_seq?}` (omit `from_seq` or `0` replays everything retained) |
| `run.cancel`    | Cancel an in-flight run — `{run_id, mode?}`; `mode: "graceful"` finishes the current turn, default aborts       |
| `run.inject`    | Inject input into the live run — `{run_id, queue, input}`; `queue: "steer"` changes course inside the current exchange, `"next_turn"` is consumed at the next turn boundary, `"follow_up"` starts a new exchange once this one finishes |
| `tool.approve`  | Approve a pending tool call — `{tool_call_id, scope?}`; `scope` widens an `exec_command` approval's trust: `"once"` (default), `"same"` (this exact command, for the session) or `"all"` (every command) |
| `tool.reject`   | Reject a tool call — `{tool_call_id, reason?}`                                                                  |

### Server → Client

| type                    | Description                                                                                                                                             |
|-------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `run.started`           | Run begun — `{run_id, session_id, input, attachments?}`; `input` is the user prompt, so a browser that didn't send it can render the user bubble, and `attachments` its image refs (`{id, url}`) for the same reason. A background task run additionally carries `{task_id, parent_session_id, parent_run_id, tool_call_id, label}` — clients key task state by the durable `task_id`, route events by `run_id`, and send it to the parent session's task list, never a chat timeline |
| `run.agent_start`       | Agent taking its turn — `{run_id, agent_name, agent_config_id?}`; the id names the config behind the agent                                              |
| `run.step`              | Streaming text delta — `{run_id, delta}`                                                                                                                |
| `run.reasoning`         | Streaming reasoning delta — `{run_id, delta}`                                                                                                           |
| `run.message`           | One completed assistant message: a turn's full text, interim narration or final answer, authoritative over its `run.step` deltas — `{run_id, text}`     |
| `run.reasoning_item`    | One completed reasoning block: a turn's full thinking text, authoritative over its `run.reasoning` deltas — `{run_id, text}`                            |
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
| `run.cancelled`         | Cancelled — `{run_id}`                                                                                                                                  |
| `session.title_updated` | Title changed — `{session_id, title}`                                                                                                                   |
| `task.updated`          | A background task moved — the task row (`task_id`, `status`, `kind`, `state`, `attempt`, `dismissed`, a paused one's `pending_call_id`…) as the store has it; on the task's run stream when the hub holds that run, else broadcast to every connection |
| `session.project_bound` | The session's first project-carrying run permanently bound its project — `{session_id, project_id}`; published exactly once, by the run that won the bind |
| `trace.span`            | Trace span — `{run_id, trace_id, span_id, error?, data?, payload_omitted?, ...}`; `payload_omitted` says the 256KB live cap replaced the payload fields, which the stored row still has |

Generation spans carry the full model request/response in their `data`
(`model`, `system_instructions`, `input`, `tools`, `model_settings`,
`handoffs`, `output_schema`, `output`) — the trace panel renders these when
you expand a generation span, so you can see exactly what each call sent
after compaction/filters, including MCP/skill tool definitions. Those payload
fields are nearly all of a session's trace bytes (every generation span
carries the whole conversation as its input), so they are not on the span
row: each element — an input item, a reply item, a tool definition, the
system prompt — is stored once per session in `trace_blobs`, gzip-compressed
when that is smaller, and the row references it by hash (decisions §5.50).
The panel opens with the SUMMARY listing (`?summary=true`: rows without the
payload, marked `payload_omitted`) and fetches one span whole
(`GET /sessions/:id/traces/:span_id`), its payload rebuilt into `data`, when
it is opened — what a session's history costs to open does not grow with
what its model calls carried. An element past `trace_span_data_kb` is
replaced with a marker string in its place (an oversized input item becomes
a string in the `input` array; its siblings stay); an element whose blob was
pruned reads as `[omitted: the stored payload was pruned]`. The `trace_include_sensitive_data`
setting (default on) keeps conversation content out of traces entirely when
off; the server always passes its resolved value explicitly as
`Observe.IncludeSensitiveData`, which is the SDK's one authority for it — the
SDK reads no environment variable (spec §2.14).

### Terminal endpoint — `GET /ws/terminal`

One interactive sandbox terminal per connection, deliberately separate from
the `/ws` event bus (different delivery semantics: an ordered byte stream with
backpressure, not broadcast envelopes with replay). Authentication is the same
first-message `auth` handshake.

After `auth.ok` the client sends exactly one control envelope and the server
answers:

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
