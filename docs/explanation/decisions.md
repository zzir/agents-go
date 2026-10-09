# Design decisions

Decisions that have been discussed and settled, each with the reason it is what
it is. **Read the rationale before reopening one.** The section numbers are
permanent addresses — code comments cite them as `decisions §5.29`, so a number
is never reused or renumbered; a retired decision keeps its heading as a
tombstone.

Every entry has one shape: **Decision**, **Rejected** (each alternative and the
one reason it lost), **Cost accepted**, and — when the rules the decision
produced live elsewhere — a closing `Rules:` line naming the
[spec](../reference/spec.md) section or the [workbench
invariant](workbench-invariants.md) that holds them. What the project
deliberately does not do lives in [scope](scope.md).

---

## 5. Recorded design decisions

**A decision is only as good as the reason recorded under it.** Entries whose
stated reason is a citation of another codebase rather than a property of this
one get marked **🔁 reason under review**: the decision stands, but it may not
be closed by citation — re-deciding one means replacing the citation with a
reason that stands on its own, or changing the decision, and dropping the mark
in the same change. Every entry below currently carries its own reason.

### 5.1 Handoffs stay; graph orchestration does not replace them

**Decision.** A handoff is "switch agent at runtime"; a graph is "declare the
topology up front". They solve different problems, and a handoff's
`InputFilter` and history folding need a lot of glue in a graph model.

**Rejected.** Graph orchestration as the multi-agent primitive — if it ever
arrives it layers *above* handoffs, for task orchestration, not agent switching.

Rules: spec §2.4; the non-goal is [scope §1.2](scope.md#12-non-goals).

### 5.2 Names describe the thing, and renames are batched

Retired as a ledger 2026-09-04; the renames rode the v0.3.0 window and live in
the release notes. What survives: a name earns a rename only when it
misdescribes or breaks a Go rule, never to "look less like Python", and a
rename is a breaking change batched into one window (§5.8) — an openai-go
major, if one comes, rides in it (§5.5b).

### 5.3 `Instructions` is a func type

**Decision.** `Instructions` is a **func type**: `StaticInstructions` covers
the fixed case, `WrapInstructions` composes, and nil handling is the runner's
job behind unexported entry points. Prompt text lives in `Instructions` alone.

**Rejected.** Single-method interfaces with `...Func` adapters — a plug point
nothing ever plugged into; a single-method injection point is a func type
unless a second method is in sight (so `tasks.AgentResolver`, `Launcher`,
`Stopper` and `WakeGuard` are funcs; `tasks.Store`, multi-method, stays an
interface). A stored-`Prompt` half — retired with OpenAI's shutdown of
reusable prompts.

**Cost accepted.** A program that bound a stored prompt pastes its text into
`Instructions`; variables become string formatting on the caller's side.

### 5.4 A tool is a struct, not an interface

**Decision.** `*Tool` is the tool type; there is no `Tool` interface. That is
how the "no hosted tools" line ([scope §1.2](scope.md#12-non-goals)) is
enforced: nothing to implement. The fields are exported, so a variant is a copy.

**Rejected.** A sealed interface with an unexported marker method — it closed
the kind just as well, but invited a wrapper hierarchy for optional behavior,
which then needed a lookup protocol to be usable.

Rules: spec §2.7c.

### 5.5 Internal item types are Responses wire types

**Decision.** Zero conversion, zero information loss — reasoning ids,
`encrypted_content` and strict schemas all survive round-trips.

**Cost accepted.** Non-LLM entries need a `session.Entry` wrapper to have
somewhere to live, and the coupling in §5.5b.

### 5.5b The wire types couple our compatibility to openai-go's

**Decision.** `InputItem` and friends are **type aliases of `openai-go/v3`
union types**, so an openai-go major bump breaks this whole API and is the
merge window for every shelved break (§5.8); within a major the provider SDKs
(openai-go, anthropic-sdk-go) track their latest release, read from the changelog.

**Rejected.** Wrapping the wire types behind our own structs — it costs the
round-trip fidelity §5.5 exists for, plus a conversion layer chasing every
Responses API addition forever. Holding a low floor behind a compat shim — the
fixes it skipped never reached users (an SSE keep-alive failed every stream
from a server that sends one), and the skipped changes piled up.

**Cost accepted.** A bump raises every consumer's floor, and a v3 minor that
retypes a field (v3.54 retyped a function_call_output's `CallID`) is a source
break, so a bump ships in a minor (§5.8); behavior a bump takes away is restored
in the adapter (spec §2.15), never by pinning. A CI job builds against `@latest`.

### 5.6 Background work runs in-process, not in isolated processes

**Decision.** Background sub-agents ("tasks") run as nested runs inside the
same process, each with its own hidden session, reporting back by injecting a
notification into the parent session.

**Rejected.** One supervised OS process per session over a line protocol — it
buys crash isolation and independent working directories at the cost of IPC,
serialization and a second lifecycle; nested runs already give independent
sessions and configuration, and the isolation is not worth the machinery at
this scale.

**Cost accepted.** A process exit — a crash or an OOM kill — ends every task
in flight with it.

Rules: spec §2.13.

### 5.6b Tracing stays vendor-neutral; OTel export is the consumer's job

**Decision.** The core `tracing` package has no dependencies: a span is a flat
record with OTel-width string ids and a `Data` map, and export is a
consumer-side `tracing.Processor`.

**Rejected.** Emitting OTel spans from the core — a heavy, fast-moving
dependency in every consumer's build for a feature most do not use. An
in-repo exporter submodule — zero consumers, since the workbench reads spans
through its own store (§5.23).

**Cost accepted.** An exporter that groups by trace must carry workflow
metadata across an N-handoff run's N+1 parentless spans itself.

Rules: spec §2.11e; [Tracing](../howto/tracing.md).

### 5.7 A submodule exists only to keep a heavy dependency out of the core

**Decision.** The repository is a Go workspace — a root module (the SDK) plus
submodules — and the **only** reason to split a module out is a heavy dependency
it would otherwise pull into the core; anything dependency-free stays in root.

**Rejected.** Splitting by cohesion — `mcp` is a module because
`modelcontextprotocol/go-sdk` brings a raft of indirect requirements, and for
no other reason; the `agents.MCPServer` inversion means the split moved no
import path.

**Cost accepted.** A submodule is released separately: each release tags
`<dir>/vX.Y.Z` beside `vX.Y.Z` on the one commit whose `go.mod`s require the
root at that version (the `replace` stays, for CI), and a between-release
pseudo-version may not build against the root it names.

Rules: [Architecture](architecture.md#module-boundaries).

### 5.8 Public API compatibility begins at v1.0.0

**Decision.** Before v1.0.0 a release may break exported identifiers; a break
bumps the minor and a patch carries only fixes and additions, and breaks are
batched into as few releases as the work allows, each recorded in the release
notes with the old spelling beside the new.

**Rejected.** A deprecation cycle before v1.0.0 — promised once and not kept
through the structural collapses; a rule nobody follows teaches the reader
this document describes intentions, and the cycle begins when the API stops
finding its shape. Letting a patch break — `go get -u=patch` is the one
upgrade Go users treat as safe.

**Cost accepted.** A fix that needs a break waits for the next minor or ships
as one. `scripts/release-check.sh` and `release.yml` refuse a breaking patch —
after the module proxy has it, so the patch is retracted and the minor tagged
on the same commit. A constant's changed value passes.

### 5.9 A parent-linked checkpoint chain for execution state is declined

**Decision.** No second history structure beside the session tree: the tree IS
the parent chain (spec §2.5d), `RunState` serializes the one state that cannot
be rebuilt — the pause awaiting approval — and per-turn persistence plus
repair (spec §2.5h) bound crash loss to the in-flight turn.

**Rejected.** A parent-linked checkpoint per superstep, browsable as a tree
(agent-framework-go's design) — it needs one because its session is a key-value
bag with no other history; here the net gain is deterministic replay, not worth
a second structure with its own consistency rules. The repair deciding which
dangling calls to redo — stored history holds a tool NAME and only the caller
knows the agent, so `RecoveryPolicy.RetrySafe` is the caller's.

**Cost accepted.** No time-travel debugger. Revisit only with a concrete replay
need, and then a checkpoint is a session ENTRY kind (a trimmed `RunState`,
projected to nothing), a deterministic execution mode comes first, and the
payload is trimmed — a per-turn copy of every raw response grows quadratically.

### 5.10 Non-Responses backends adapt at the model boundary

**Decision.** The canonical format stays Responses (§5.5) whatever the backend
speaks: an adapter translates both ways **inside its own package**
(`models/anthropic`), so nothing outside it learns a second format, and
`models/modelkit` holds the shared halves plus the conformance suite every adapter runs.

**Rejected.** A second canonical format, or a neutral abstraction both backends
map onto — a lowest-common-denominator model loses exactly the Responses
semantics (reasoning ids, encrypted content, strict schemas) the SDK guarantees
depth on. Chat Completions as the second backend — [scope §1.2](scope.md#12-non-goals).

**Cost accepted.** Each adapter re-implements the translation and alone knows
what its backend cannot express — which is why an unsupported feature must
fail loudly rather than drop silently.

Rules: spec §2.15; the Anthropic mappings are in [Models](../howto/models.md).

### 5.11 Construction errors split by data provenance

**Decision.** A constructor whose failure can only be a programmer error
**panics** (`NewTool`, `AgentAsTool`, `OutputType`: a schema derived from a Go
type is deterministic per type); one whose input is runtime data **returns an error**.

**Rejected.** Returning an error from a type-derived constructor — the failure
is a bug any test surfaces at once (the `regexp.MustCompile` precedent), and
the error return would cost chaining inside `Agent{...}` literals. Returning a
tool that errors on every invocation, surfaced by the runner before the first
model call — it deferred a deterministic bug to runtime and cost a field plus
a runner check. Letting `Tool.NonStrict` rescue
a type strict mode cannot express — it relaxes a tool that already exists,
while the strict schema is built during construction; hence the twins
`NewToolNonStrict` / `OutputTypeNonStrict`.

**Cost accepted.** `AgentAsTool` has no non-strict twin — a recorded gap, not
a decision: no caller has needed an unconstrained field in a nested run's
arguments, and until one does the way out is building the `Tool` value directly.

Rules: spec §2.7h, §4 (strict schemas).

### 5.12 One user-context entry point

**Decision.** `RunOptions.Context` is the only way user data enters a run;
every run wraps it in a fresh `RunContext`. Nested runs share the parent's
`Context` value with fresh accumulators, and cross-run usage totals are sums
over each `RunResult.Usage`.

**Rejected.** A field to inject a pre-built `RunContext` — two fields for one
concept, and a run owning its `RunContext` outright is what the guarantee "a
run's accumulators start empty" rests on.

### 5.13 AgentToolConfig configures the tool, ModifyRunOptions the run

**Decision.** `AgentToolConfig` holds only what has no `RunOptions` counterpart
— the tool's name, description, visibility, approval gate, error rendering,
output extraction, streaming callback and input rendering; everything about the
nested run itself goes through the single `ModifyRunOptions` channel.

**Rejected.** Mirror fields (`MaxTurns`, `Session`, `ConversationID`) — each
was a second spelling of a `RunOptions` field, and the escape hatch's
existence proved the dedicated-field approach could never be complete.

**Cost accepted.** A `ConversationID` set via `ModifyRunOptions` is cleared
when a paused nested run resumes; the serialized state already carries the
conversation.

### 5.14 Sandbox file tools share exec's path view

**Decision.** The file operations resolve paths with shell semantics,
identical to `exec_command`: the isolation boundary is the sandbox, not the
working directory — exec already reaches the whole filesystem, and the model
echoes the absolute paths it learns from exec output into the file tools.

**Rejected.** A workdir-rooted "virtual chroot" — absolute paths got re-joined
under `WorkDir` and read as "not found". Docker's archive API (`docker cp`)
for persistent containers — it cannot see a tmpfs mount, so a file exec had
just written to `/tmp` read back as absent. An interface over the docker
backend's three file dispatches — never chosen dynamically, it would hide
which one runs without removing a branch.

**Cost accepted.** Docker bind-mount mode is the one exception: its file
operations run on the host side of the mount, confined to `WorkDir` via
`os.Root` (spec §2.7t).

Rules: spec §2.7t.

### 5.15 Streaming-only backends adapt with a Model decorator

**Decision.** A backend that refuses non-streaming requests (the ChatGPT Codex
backend's 400; anthropic-sdk-go's client-side cap on `max_tokens` above 21,333)
is adapted by `NewStreamOnlyModel` / `NewStreamOnlyProvider`, composed innermost
so retry, fallback and routing see a severed stream as an ordinary `Respond`
error.

**Rejected.** Forcing `"stream": true` as an HTTP middleware — it hands an SSE
body to a caller that parses a JSON response; the request shape and the
response parser must switch together, which only the model boundary sees.

**Cost accepted.** The assembled response carries no `RequestID`, and a
length-truncated `response.incomplete` counts as arrived, not failed — the
same as the runner's streaming path.

Rules: spec §2.15.

### 5.16 A severed stream retries only before output, with the preamble held back

**Decision.** `NewRetryModel` and `NewFallbackModel` may replace a broken
streaming attempt only while nothing the model **generated** has been
delivered; lifecycle preamble and terminal-failure events carry nothing
generated, so they are buffered rather than delivered.

**Rejected.** Retrying after output — a delivered event commits the consumer
to a response a second attempt then duplicates. Committing on the preamble —
`response.created` arrives the moment the connection opens, making every
severed stream unretryable. Treating a clean EOF at an event boundary as a
finish — accurate but unretryable; the runner keeps that check only as the
last line of defense. Failing a call on a transport error AFTER the terminal
event — a complete, valid result thrown away over a connection with nothing
left to say.

**Cost accepted.** A `response.incomplete` commits, so a retry never rescues
one; every decorator that saw a post-commit error records it, so a nested
chain accounts for one break once per layer.

Rules: spec §2.7e.

---

### 5.17 The session layer is its own package

**Decision.** `agents/session` owns stored history and never imports the
runner; the one upward need, `EntryFromRunItem`, stays in agents. The value
types both layers share (`Source`, `ItemDisplay`, `RequestUsage`,
`Diagnostic`, `ErrorCode`) live in session, **aliased** in agents under the same names.

**Rejected.** Aliasing session-only names into agents — code that works with
stored history imports the package that owns it. Renaming an alias — the
compile error, the godoc and the reflected name then disagree with the code.
Dropping the `session.Session` stutter — the concept IS the package, as with
`context.Context`. Deriving `ErrorCode`s in session — `CodeOf` and `Classify`
read error types that are agents'. Reading history in a row's own time-ordered
key (a UUIDv7) — a clock can step back; `Seq` is the only order.

Rules: spec §2.5c, §2.5e2.

### 5.18 A RunState decodes across a version window, and the window is earned

**Decision.** `RunStateFromJSON` accepts a window of schema minors: a pause
waits on a human, the process may be redeployed meanwhile, and refusing the
state afterwards strands the run for a reason the user had no part in. The
floor is 4 — `"1.3"` was stamped on two incompatible payloads.

**Rejected.** Strict version equality — the field-by-field fallbacks were cost
with no payer, and an equality gate destroys states an additive bump resumes
fine. A retroactive window — a reinterpreted field decodes *successfully* with
its meaning dropped, worse than a refusal, since the caller is told the resume
is faithful. A side channel for a host's pause-scoped state — a build-time
transform (`middleware.Plan`) returns fresh state on rebuild, so what it knew
rides `RunState.Extra` with the pause instead.

Rules: spec §2.1 (`RunStateVersionSupported` is the consumer's gate).

### 5.19 A named container is adopted only against a configuration fingerprint

Revised 2026-08-24 with derived container names (§5.28).

**Decision.** A persistent docker sandbox adopts a container holding its name
only on a matching ownership label and fingerprint; ours with another
fingerprint is **replaced**, since with `KeepOnClose` every config edit would
otherwise strand its old container on the derived name forever.

**Rejected.** Matching on image + mount alone — a container created under a
laxer policy (network on, root, no limits) passed both checks and silently
served a config that no longer allows any of it.

Rules: spec §2.7n.

### 5.20 A shared connection is not a caller's to cancel

**Decision.** A request rides the connection's context, and the caller's
cancellation is honored by returning from the wait: the session is shared by
every run on that server, the context belongs to one. Generalized, **a
resource shared between runs is never handed a single run's cancellation**.

**Rejected.** Issuing each request on the caller's context — the go-sdk's
streamable HTTP transport fails the whole CONNECTION when one request is
cancelled mid-flight (a `sync.Once` closing its failure gate), so one person
stopping one run was observed failing five background tasks across two
conversations, each blamed on its own agent's server.

**Cost accepted.** One in-flight request outlives its caller, bounded by the
connection's lifetime and a request ceiling generous enough to fire only on a
request that is already lost.

Rules: spec §2.16.

### 5.21 A dead shared connection repairs itself, and the redial never repeats a tool call

**Decision.** The go-sdk re-establishes nothing (it only resumes an interrupted
SSE stream), so the connection owns its recovery, in place for every holder;
the redial repeats only idempotent work, because a dead line cannot say whether
the server ran a tool, and running a write twice is worse than reporting it once.

**Rejected.** Reconnecting without `Redial` — only the configuration's owner
can rebuild a transport (an `*exec.Cmd` is spent once; an endpoint needs its
headers, proxy and OAuth handler).

**Cost accepted.** Recovery is opt-in; without `Redial` the failure is reported.

Rules: spec §2.16.

### 5.21b An MCP retry waits on the transport, never on an answer

**Decision.** A retry answers a transport failure, never an answer the server
sent (the same bytes earn the same refusal), and a `tools/call` only when the
dial failed, the one failure proving the request never left; the capped,
jittered delay keeps a shared server out of lockstep and lets `-1` be a real setting.

**Rejected.** Sharing the model layer's `RetryPolicy` — its `DefaultRetryIf`
(retry everything but cancellation) is exactly the policy that made an
infinite MCP retry indistinguishable from a hang. An uncapped exponent — a
one-second base sleeps half an hour by the twelfth attempt. Retrying a call on
the tool's `readOnlyHint` — an outside claim (§5.53).

**Cost accepted.** A call the server never received because the connection
died after the dial is not retried either; the model hears about it and decides.

Rules: spec §2.16.

### 5.22 Retry policy lives in one layer

**Decision.** The one retry layer for a model call is `NewRetryModel` —
provider-agnostic, classifiable (`RetryIf`), observable (a span per attempt) —
so the providers build their clients with `WithMaxRetries(0)`;
`modelkit.RetryableError` is the classifier each adapter wraps, `agents.DefaultRetryIf` the coarser SDK-level default.

**Rejected.** Letting both layers retry — the official clients' default two
attempts and `NewRetryModel` compose multiplicatively, and neither can see the
other. Clamping a `Retry-After` longer than `MaxDelay` to the cap — a wait the
caller capped below what the server asked for is a signal to stop. Treating
`DeadlineExceeded` as fatal — it is usually the attempt's own budget, the
hung-request case retrying exists for, and a caller's deadline stops the next
wait anyway. Letting the status outrank an explicit `X-Should-Retry` — the
server knows whether THIS failure is transient, and only its two exact values
carry meaning. Treating `io.ErrUnexpectedEOF` as fatal — a gateway severing an
SSE stream arrives as a plain io error, not a `net.Error`.

Rules: spec §2.16.

### 5.23 Zero-consumer surface was cut to the workbench's actual needs

Retired 2026-08 as a ledger of cuts (`tracing/otel`, `filesession`,
`tools/bravesearch`, `cmd/verify`, MCP serve) under the standing rule that a
zero-consumer feature is removed, not kept. What survives: `internal/agentstest`
is test infrastructure, not API — testing against the SDK means implementing
`agents.Model` — and docs sync inside the change that moved the code.

### 5.24 The workbench has no provider routes

Decided 2026-08-24.

**Decision.** An agent names its provider (`provider_id`), full stop;
cross-provider mixing inside one run is `fallback_models`, per agent and visible
in its config. `RouterProvider` stays as SDK API for embedders; the workbench
never builds one.

**Rejected.** A global prefix→provider table (`provider_routes`) — two
selector surfaces for one decision, and the global one silently overrode the
agent's; a future need for shared endpoint selection belongs on the provider rows.

### 5.25 The workbench speaks one MCP transport: streamable HTTP

Decided 2026-08-24.

**Decision.** `McpServerConfig` carries no transport discriminator: `Config` IS
an `HTTPMcpConfig`, and a stdio-only server joins through a stdio→HTTP proxy the
operator runs outside the workbench's authority. The SDK's `mcp` module keeps
its stdio transport — an embedder spawning a subprocess is their own trust
decision.

**Rejected.** Stored stdio servers — arbitrary command execution on the host as
the server's process user behind one admin write, and the blocker for ever
letting non-admins configure MCP; a sandboxed variant is a new decision argued
here first.

Rules: [MCP servers](../reference/protocol.md#mcp-servers--apiv1mcp-servers).

### 5.26 A skill is one SKILL.md document

Decided 2026-08-24, a deliberate narrowing of the Agent Skills format.

**Decision.** A skill is the SKILL.md document alone — no bundled `scripts/`,
`references/` or `assets/` — living in the workbench's database like every
other configuration entity; the SDK's `skills` module is storage-free primitives
(`Parse`, `RenderIndex`), and activation is a `read_skill` tool the caller
provides.

**Rejected.** The full format's file trees — they force skills onto a
filesystem, and a path-based reader needs an os.Root confinement the document
model does not. Per-skill file storage, ever — a skill needing an artifact
inlines it or instructs the model to fetch it.

**Cost accepted.** A skill that instructs the model to run bundled scripts
does not get them; references to a repo's other files dangle, and the model
writes its own code in the sandbox. Import URLs are member-supplied outbound
requests with no SSRF defense — §5.29's accepted risk.

Rules: [Skills](../reference/protocol.md#skills--apiv1skills) in the wire surface.

### 5.27 The workbench's sandbox is Docker, and SSH is how a remote daemon is reached

Decided 2026-08-24; revised 2026-08-28, see §5.36.

**Decision.** A workbench sandbox is a Docker container or, since §5.34, an
E2B-compatible one; for Docker what varies is WHERE (`host` empty, `ssh://`,
`tcp://`), and SSH lives on inside `sandbox/docker` as a TRANSPORT — a pure-Go
dialer to the remote `docker.sock` over one shared, self-healing connection.

**Rejected.** A `local` sandbox type — host execution behind one admin write
and one approval, and the reason a web terminal had to be special-cased off;
`sandbox.LocalSandbox` stays for embedders and tests, never offered by the server.
A generic `ssh` sandbox — a login user's full privileges, no limits, on a
machine the server merely had credentials to; raw remote exec is
x/crypto/ssh, the value here is the sandboxing SSH never provided. A remote
docker CLI or a local ssh binary as the transport — the server shells out to
nothing. Healing a rejected channel open — it arrives on a healthy transport,
and reconnecting would sever every terminal multiplexed on the same client.

**Cost accepted.** An isolation need beyond containers (VMs, gVisor) is a new
backend decision argued here first — gVisor is already reachable via
`runtime: runsc`. Do not reintroduce a host-exec or raw remote-exec type.

Rules: workbench invariant 45 (only a sandbox's identity freezes); invariant 27
(a session binds a project);
[Sandboxes](../reference/protocol.md#sandboxes--apiv1sandboxes).

### 5.28 A project is the unit of working storage, and containers are per project

Decided 2026-08-24; revised 2026-08-25 (§5.29), 2026-08-28 (§5.33, §5.36)
and 2026-08-29 (deletion contracts in SQL).

**Decision.** A **project** is one user's working tree on one sandbox, keyed by
id and never by a typed path, with one persistent container per project —
derived-named so restarts re-adopt (§5.19), kept stopped so installed packages
survive idle. The affinity is deliberate: a tree that could "move" between
daemons would silently be two sets of files.

**Rejected.** A free-form working directory per session — it made "which tree
did that command touch?" a question with a surprising answer. Cascading a
sandbox delete onto its projects — a project delete reclaims storage, so the
cascade would destroy working trees as a side effect of removing a machine.
Projects behind the admin gate — they are the first PERSONAL configuration
entity, scoped in the handlers; an admin MANAGES the plane (§5.29).

**Cost accepted.** Deletion and binding contracts settle in SQL per dialect —
single-statement guards on SQLite, parent-row locks on PostgreSQL — two shapes
to keep equivalent. Sessions, forks and tasks on one project share its tree; a
regenerate or fork rewinds the transcript, not the files.

Rules: workbench invariant 27 (binding, fences, locks); the operational
surface, the terminal's ownership rule included, is
[Projects](../reference/protocol.md#projects--apiv1projects).

---

### 5.29 Configuration is scoped per row: private to its owner or global

Decided 2026-08-24; owner semantics revised 2026-08-25, listing order
2026-08-31.

**Decision.** Configuration rows carry two independent columns — `scope` decides
**who sees** a row, `owner_id` **who wrote** it, permanent across every flip —
and the write matrix follows: publishing is the admin's alone, because
publishing to every member is the review point and the reviewing role does it;
unpublishing is the admin's or the author's, and the row returns to its author,
who never left it.

**Rejected.** Admin edits on private rows — a config an admin could silently
rewrite under a member's name would blur whose credentials and instructions a
run carries. A per-row ACL — one team, one trust boundary. A same-scope flip
as a no-op — a flip is defined FROM the other scope only, so two racing
demotes cannot both flip a row. A transfer that skips validation — a config
that answers 204 and then fails every run is the state a save already rejects.
A global holder referencing private rows — promoting it would publish a config
whose parts most members cannot see. Listings ordered by scope — on the
permanent `owner_id`, only a transfer ever reorders a row.

**Cost accepted.** A member's published row stays theirs to change after the
admin approved it. Member-supplied URLs (MCP endpoints, provider base URLs,
skill imports) get **no private-network/SSRF defense**: one team, one trust
boundary, and egress control is applied outside the server.

Rules: workbench invariant 42 (the in-write re-check); the matrix, the reference
rule and list ordering are in [the wire
surface](../reference/protocol.md#authorization).

---

### 5.30 A credential lives on the row that spends it — no global fallback keys

Retired 2026-08-24 as a ledger of removed settings (`openai_api_key`,
`anthropic_api_key`, `brave_api_key`, `github_token`). What survives: a key
any keyless row inherits is the ambient authority §5.29 removed — a provider
row carries its own key or runs keyless, an agent naming no provider fails its
pre-flight, and settings' only secret is attachment storage's own S3 key.

---

### 5.31 A skill's identity carries its repository; a repo publishes as one group

Decided 2026-08-25, refining §5.29 for skills alone.

**Decision.** The repository is part of a skill's name, materialized as
`repo_label` on the row, and **a repo group is one scope and one owner**,
moved per `(source_repo, owner_id)` in one statement, every operation naming its group.

**Rejected.** Searching for a plausible group instead of naming it — an admin
holding a private copy of a repository somebody else published would refresh
their own copy believing they synced the published one. Keying uniqueness on the
raw source URL — two URLs can reduce to one label, and a duplicate qualified
name makes `read_skill`'s answer a coin flip. Merging on a transfer into an
owner who already holds a group for that repository — that is how a mixed-scope
pile forms, and the unique indexes cannot see it because they partition BY
scope. A persisted aggregate root for the group — the invariant needs one
consistent instant, not a table; only the write needs serializing.

**Cost accepted.** The author of a published repo can add global skills by
pushing upstream and syncing, without a second admin act — accepted on the
one-team trust boundary (§5.29). The same repo imported by two people is two
groups whose qualified names collide and resolve own-over-global.

Rules: [Skills](../reference/protocol.md#skills--apiv1skills) in the wire surface.

### 5.32 A project's environment is write-only, like every other credential here

Decided 2026-08-26.

**Decision.** A project's environment is sealed at rest, masked in every
response, replaceable but never readable back; names stay plaintext so one
variable can be rewritten alone and the audit log stays answerable, and the
seal's AAD is the project id, so a ciphertext moved to another project is not a
decryption oracle.

**Rejected.** A per-entry "secret" flag masking only marked values — it would
make this the ONE credential surface whose visibility is a per-item choice,
against provider keys, MCP secrets, SSH passwords and trigger secrets, all
unconditionally write-only; and a forgotten flag writes a token to a readable
field silently, a failure with no upper bound.

**Cost accepted.** Confirming that `TZ` says what you think takes one `env` in
the terminal the workbench offers — the honest place, since the environment is
readable to everything in that container anyway; sealing defends the database
and the screen, never hides a value from the model, and the UI says so.

Rules: workbench invariant 27 (the environment is content, not identity).

### 5.33 Storage is a volume the delete destroys

Decided 2026-08-28. The target/template split this entry introduced was
reversed the same week — §5.36 holds what replaced it. What stands:

**Decision.** The runtime generation lives only on the PROJECT, bumped by
every content change to its sandbox, so the instance cache, the terminal
fences and `RetireProject` watch one thing; Docker storage is a volume the
project delete destroys, since a volume nobody has a listing for is an unbounded leak.

**Rejected.** The local daemon's bind mount (`--workspace`, the `DOCKER_HOST`
guard, the operator uid:gid default) — that default kept the container unable
to install a package into itself. A per-owner "scratch" project for unbound
runs — it made "which tree did that command touch?" surprising. A sandbox
generation beside the project generation — two maps that must not reach each
other's rows. A second binding column beside `project_id` — a project pins its
machine, so it could only disagree.

**Cost accepted.** The tree is no longer a directory on the operator's
machine; `docker cp` and the export route are how it comes out. A workbench
container runs as root unless the sandbox names a user (overriding spec
§2.7o's image-user default), with all capabilities dropped: the container is
the isolation boundary.

Rules: workbench invariant 27;
[Projects](../reference/protocol.md#projects--apiv1projects).

### 5.34 One E2B-compatible backend, written here, not one backend per cloud

Decided 2026-08-28; verified against E2B's cloud, Alibaba Cloud Function Compute
and Bailian.

**Decision.** The second backend is **one backend that speaks the E2B API**,
written here — five REST calls and a ~150-line Connect codec on the standard
library, which keeps `sandbox/e2b` in the root module (§5.7) — configured by a
row naming the service (`api_url`, `domain`, `api_key`, `headers`), with no
`flavor` discriminator.

**Rejected.** One backend per cloud — the services differ in four fields. An
auth-scheme switch (`X-API-Key` vs `Authorization: Bearer`) instead of
`headers` — Bailian wants both at once, and `headers` is the E2B SDK's own
parameter. `/timeout` for the extension — Bailian lacks it, and its 404 would
read as a dead sandbox. A community Go SDK, or a protobuf toolchain with
generated stubs — two module dependencies for six messages. A metadata query
to find a sandbox — a filter syntax the compatible services do not document
identically; the id is recorded before use instead, since an unrecorded
sandbox is billed compute nobody will ever stop. A keepalive goroutine — the
extension rides the control call the operation already forces.

**Cost accepted.** The hand-written codec is checked against a fake and a
probe, not a schema. A terminal idle past one full lease can lose its sandbox.
Pausing on Function Compute is gated behind a per-function feature, and the
client passes the service's refusal through verbatim. A service those four
fields cannot express is a new decision, not a switch to grow.

Rules: spec §2.7u; [Sandboxes](../reference/protocol.md#sandboxes--apiv1sandboxes);
the services' rendering quirks live on the code that absorbs them (`sandbox/e2b`).

### 5.35 A port preview is a gateway with a grant, not a published port (retired)

Retired 2026-08-31. Was a grant-token reverse proxy from a sandbox port to the
browser (`sandbox.PortForwarder`/`PortDialer`, `ports`). Because the compose
topology publishes on the daemon host's loopback, unreachable from a
containerized server. No browser ships: an agent installs one in a sandbox
with network on, or an operator bakes one into an image. See §5.70.

### 5.36 A sandbox is one row, and only its identity freezes

Decided 2026-08-28, reversing §5.33's target/template split the same week it
landed.

**Decision.** One `sandboxes` row carries where it runs and what runs on it,
and a project names one; the mutability line is drawn between FIELDS, not
tables — where it always was: only the type and destination ever froze, while
the credentials sat in the "frozen" table and were always editable.

**Rejected.** Separate `sandbox_targets` and `sandbox_templates`, justified
by reuse — for the common case there was nothing to reuse (a local docker
target's whole config is `{}`), and the split bred its own bug class: a
target and a template of different types, needing a type check on the project
write, another on the health check, filtered dropdowns in two places, and
still reaching a screen as `unknown sandbox target type: e2b`.

**Cost accepted.** A second image on one remote daemon repeats that daemon's
host and credential, and rotating a key touches every row that carries it.
`Duplicate` copies everything but the identity and the credential, dropped
rather than carried as a mask that would resolve to empty and look copied.

Rules: workbench invariant 45 (which fields freeze, and why e2b freezes three
more).

### 5.37 The E2B sandbox defaults to no network

Decided 2026-08-28.

**Decision.** The `sandbox` package promises isolation by default and the
docker backend keeps it, so the E2B create sends `allow_internet_access`
explicitly on every create — `false` unless the sandbox opts in.

**Rejected.** Omitting the field and inheriting the service's own default —
which is internet ON.

Rules: spec §2.7o.

### 5.38 A workbench docker sandbox caps memory and CPU by default

Decided 2026-08-28.

**Decision.** A docker sandbox whose config leaves `memory_mb` or `cpus` at
`0` gets the workbench's default cap (4096 MiB, 2 CPUs) in
`sandboxes.applyImage`: agent code runs there, and an uncapped container is a
host-DoS surface that on a shared workbench is everyone's.

**Rejected.** Putting the default in the SDK's `sandbox` package — its
isolation-by-default promise covers network, filesystem, capabilities and the
per-command timeout, and a library embedded on its own has no context to
assume it is running untrusted code for many users.

Rules: [Sandboxes](../reference/protocol.md#sandboxes--apiv1sandboxes).

### 5.39 The SDK reads no environment variable of its own

Decided 2026-08-29.

**Decision.** The `agents` package calls no `os.Getenv`; every knob is passed
in. What a wrapped vendor library or an OS tool the docker backend drives
reads is its own visible, overridable contract — the distinction is
authorship: the SDK decides nothing from ambient state.

**Rejected.** A nil toggle falling back to
`OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA` — the one place the package read
process environment, contradicting the stated stance (no global registry, no
init hook, no ambient default) and fighting the embedder, who always passed an
explicit value.

**Cost accepted.** Turning tracing content off is `IncludeSensitiveData:
new(false)`, an explicit per-run decision — which is the point.

Rules: spec §2.14.

### 5.40 A handoff acknowledgement tells the target it owns the turn

Decided 2026-08-30.

**Decision.** The acknowledgement carries the transfer marker AND a
plain-language identity line: small models act on the sentence; large ones
are unaffected by the redundancy.

**Rejected.** The marker alone — a weak model reads it as the output of a
tool *it* called and narrates the transfer in the third person (observed: a
small flash model saying it had "transferred the question" to the very agent
it had just become). Prose alone — trades a machine-readable signal that
keeps the lineage for nothing.

Rules: spec §2.4.

### 5.41 ChatGPT login redeems a pasted callback URL, not a loopback listener

Decided 2026-08-31.

**Decision.** With OpenAI's Codex OAuth client the redirect must be loopback, so
the authorize URL names `localhost:1455` but nothing listens: the user pastes
the failed redirect's URL back and the server redeems the code against the PKCE
verifier it stored by `state` — a common headless-OAuth pattern, the same local
or remote.

**Rejected.** A CLI-style listener on `127.0.0.1:1455` — deployed to a remote
host, the popup's `localhost` is the *user's* laptop, so the redirect never
reaches the server and the login silently never completes; repeated attempts
also held the port for five minutes each.

**Cost accepted.** One manual paste instead of an automatic catch — a
workbench meant to be deployed ([scope](scope.md)) cannot depend on the
browser and the server sharing a loopback interface.

Rules: [ChatGPT OAuth](../reference/protocol.md#chatgpt-oauth).

### 5.42 Image attachments live in an S3 bucket as stable public URLs

**Decision.** Image bytes go to a configured S3-compatible bucket and the
request carries a **public-read, unsigned, stable URL** under an unguessable
key; entries store a sentinel a `ModelProvider` decorator hydrates at the model
boundary — the one seam every path crosses (fresh, resume, replay, compaction,
fallbacks).

**Rejected.** The database as the only backend — every turn re-inlines every
image as base64, and a local-first server cannot hand a provider a
`localhost` URL. Presigned URLs — both providers cache prompts by prefix, so a
URL that differs per request re-bills the whole history after an image, and
an expired signature 404s a replay. Hydrating in the entry store's load — it
covers only HISTORY; the current turn's input and a resumed state's input
reach the model without a storage read. The AWS SDK — the server module's
heaviest dependency for two calls; sigv4 is ~150 lines in-repo, verified
against an openssl reference vector. A uuid v7 key — its timestamp prefix
narrows a brute-force window.

**Cost accepted.** Anyone holding a link can read that image; the setting
says so. An install without a bucket has no image input — attachment storage
is a power-user step (scope §1.1). The scheme constant lives in `store`, since
`attachments` → `settings` → `store` leaves the client package unable to own it without a cycle.

Rules: workbench invariants 56–58.

### 5.43 Config booleans are stated positively

**Decision.** Every boolean in stored configuration names the capability it
grants, and `true` turns it on; a default-on knob added late is a `*bool` where
nil means the default, so "zero value means unchanged" is done by type, not by
name.

**Rejected.** Negated flags (`disable_x`) on the config surface — they
existed only to keep a late knob from flipping existing rows, which the
pointer already does; the old keys decode past silently, the
`compaction_threshold` precedent. Negating the SDK's own fields to match — the
SDK keeps Go's zero-value idiom (`Agent.DisableToolChoiceReset`, like
`http.Transport.DisableKeepAlives`), and the bridge flips polarity in one place.

### 5.44 Middleware and sessions: the session is the memory, not the input

**Decision.** With a session attached, a re-entering middleware sends only
what the session does not yet hold; one that must know whether an attempt
stored its input reads the SDK's own announcement, `ItemsPersistedEvent`.

**Rejected.** Rebuilding the next attempt's input by hand (`Loop` feeding the
whole attempt back through `ToInputList`) — right without a session and wrong
with one: the loop prepends the session's history to every attempt and
persists the new input ahead of the first model call, so the model saw the
prompt twice and the prior turns three times over.

**Cost accepted.** Announcing the user-input save widens an existing
contract; the one consumer that mirrors persisted state from it (the
workbench's stream bridge) resets buffers that are still empty.

Rules: spec §2.5, §2.12.

### 5.45 A middleware resumes under the caller's control

**Decision.** `RunInput` carries the caller's `RunControl`, and `ResumeRunWith`
continues a paused run under it, so an in-chain resume leaves the handle `Run`
returned live; the queue is carried as is, since the pause copied it into the
state without draining it, and a reseed would deliver every item twice.

**Rejected.** Resuming through `ResumeRun`, which mints a fresh control —
correct for a serialized state in a new process, wrong in-chain: every
`StopAfterTurn` or `Steer` on the original handle after the first policy
resume reached a run that had already ended, and the caller had no way to
know. Accepting a host-built control — the interface exists so a host can
hold one, not implement one.

**Cost accepted.** `Loop` needs `RunResult.StoppedEarly` to tell "finished"
from "stopped", so the stop is never cleared and is reported wherever the run
ends.

Rules: spec §2.11b, §2.12.

### 5.46 A tool panic takes the tool-error path

**Decision.** The recover lives in `invokeTool` on both the timed and untimed
paths, so a panic is an error from the call like any other and takes the one
error tail; the per-call goroutine's own recover is only a net for a panic
outside the tool body.

Rules: spec §2.2 (concurrency), §2.7 (Errors).

### 5.47 Zero-setter sandbox options were removed

Retired 2026-09-02. Was `ExecRequest.Stdin`, `docker.Options.ContainerWorkDir`
and `ExportTar`'s path parameter, none with a setter. Because a zero-consumer
option is removed, not kept; each returns with its caller. See §5.23.

### 5.48 apply_patch parks a large file instead of snapshotting it

**Decision.** apply_patch's atomicity rests on an in-memory snapshot of every
file it touches, so a failed commit rolls each back; a Delete of a file over
the read limit — the one operation that needs no content — is parked by an
atomic rename instead of refused, which is why `Sandbox.Rename` stays on the interface.

**Cost accepted.** Update and Move are not parked: they need the content, and
the read limit is the limit.

Rules: spec §2.7s.

### 5.49 The Anthropic adapter decides its output items at the stop reason

**Decision.** Three translation choices share one reason: the runner reads a
turn as ONE assistant message and executes its tool calls before it looks for a
refusal, while the Messages API reports its verdict LAST — so text blocks merge
into one item, `output_item.done` waits for `message_stop`, and a replayed
`refusal` part is dropped.

**Rejected.** A message item per text block — the runner keeps only a turn's
last message, so every text but the last was silently dropped. Per-block done
events — they leaked a `function_call` done for a response whose stop reason
turned out to be `refusal`, an item the terminal output rightly did not
carry, breaking the contract that the two are interchangeable. Replaying a
refusal part as assistant text — a refusal is not an answer the model gave,
and replaying it as one teaches the next turn that it was.

**Cost accepted.** Finished items wait for the verdict; text deltas still
stream live. Under a refusal, the items announced past index 0 get no
`output_item.done`, and index 0's finished type may differ from the announced one.

Rules: spec §2.15; the mappings and the other lossy translations are in
[Models](../howto/models.md#what-the-translation-does).

### 5.50 Trace payloads are content-addressed per session, not stored per span

Decided 2026-09-02.

**Decision.** A span's payload elements are stored once per session in
`trace_blobs` under their sha256, the span row keeping metadata plus a packed
list of hashes; blobs are keyed `(session_id, hash)` and never shared, because
every lifecycle operation is whole-session and no reference count or sweep exists.

**Rejected.** A payload per span — every model call re-stored the whole
conversation, so a session's trace grew with the square of its length (a
200-turn session with 1 KB items ran to some 60 MB of `input`), and the row
cap bounded one span, never the sum. Global content addressing — it would
dedupe tool schemas across sessions, but needs a reference count or
mark-and-sweep with a concurrency story, and muddies per-user erasure. A
reference table — a row per reference costs ~150 bytes with its index, five
times the hash it points at. Splitting elements by item type — split by shape
(an array is one element per item, anything else one), the store knows
nothing of the SDK's item types.

**Cost accepted.** A copy of the tool schemas per session — kilobytes,
against the quadratic term removed. The replay body cap no longer derives
from the span cap and is a constant.

Rules: workbench invariant 62.

### 5.51 Off-chain history is decided by position, not provenance

Retired 2026-10-03. Was `CompactionArgs.OffChainItems` and
`RunState.OffChainHistory`: what a rewrite built from a server response chain
would delete, reported for `openai.CompactionSession`. Because that session
went — the one path that rewrote history — and nothing else read the flag;
an older state's flag is ignored and the schema floor stays. See §5.52.

### 5.52 Overflow recovery writes on the side the pass can survive

Decided 2026-08.

**Decision.** With a `Compactor` the turn is written BEFORE the pass — it reads
the log, so the turn has to be in it; with a `CompactionAware` storage AFTER —
its replacement may keep nothing of the newest turn, so a write made first is
stored, counted delivered by that very write, then gone with nothing in flight
to roll back.

**Rejected.** Treating every 400 as an overflow — it would compact and retry
after a malformed request, hiding a bug behind a shrinking conversation.
Writing the turn when no recovery is available — there is no pass to prepare
for, and the write would only spend the rollback the failing run is about to
want. Retrying on a no-op pass — an identical request fails identically.
Retrying before the turn is written — the model would get a conversation the
caller's steer never reached, while the next write past its mark counts it
delivered (spec §2.11b). Judging a forced pass by entry count or by "did
anything change" — a storage that abandons its replacement mid-pass leaves one
extra entry, pushing the oldest out of a saturated window: the same LENGTH,
and an append that reads as "changed"; only "strictly less bytes" rules the
no-op out, bytes being a deliberately conservative proxy for tokens.

**Cost accepted.** `MaxRetries` is zero by default, so an overflow is reported
unless the caller opts in; a write that fails abandons the recovery with a
`compaction_failed` diagnostic rather than retrying blind; a pass whose result
does not weigh less costs a retry the run would have spent on a request that
already failed.

Rules: spec §2.5g, §2.15

### 5.53 Plan mode denies rather than hides

Decided 2026-08 with the `Plan` middleware.

**Decision.** A gated tool stays in the toolset and answers with a refusal
naming `submit_plan`, as a tool OUTPUT: a model carries priors about tool NAMES
and reaches for them unprompted, so a hidden tool is called anyway and "not
found" teaches it nothing; an error would abort the run, and a phase decision is
not a failure.

**Rejected.** Hiding gated tools. Denying handoffs the same way — a target's
full toolset is a side door out of plan mode, and a model has no priors about
THIS agent's targets, so hiding one wastes no turn; the cost lands on the
prefix (spec §2.12). A second pause mechanism for plan review — `submit_plan`
is an ordinary approval-gated tool. A session-scoped phase in the SDK — the
SDK has no notion of a session; `OnUnlock` and `Unlock` let a host keep its
own record. Trusting `readOnlyHint` — an outside server's claim about itself.
The approval ledger as the unlock record — it records approvals whose
execution then failed, so the host persists the UNLOCK itself. Letting the
model decide whether to plan — "simple, no plan needed" is the failure the
gate exists to catch (workbench invariant 33).

**Cost accepted.** A gated write tool spends a model turn on a refusal. A
read-only tool named in `ApproveTools` keeps its approval in both phases.
Nothing checks that a tool claiming read-only behaves.

Rules: spec §2.12; workbench invariants 33, 89.

### 5.54 A task's ending is claimed, not observed

Decided 2026-08 across the task_retry, task_stop and workflow-as-task work.

**Decision.** A task's ending is won by compare-and-set, and every consumer
acts on the transition it claimed, never on a row it read: a read-modify-write
cannot arbitrate two finalizers, and once a failed task can be reopened for
retry, "non-terminal means the current run" no longer holds.

**Rejected.** Cancelling on the row alone — a stop that read the row just before
a retry would cancel the new attempt while its run kept executing, unkillable,
and an approval persisted before its pause landed would pause, reclaim or reap
the attempt that replaced it. A stop that only answers success — a host asked to
stop a run it has never heard of (ordinary during a launch) reads "it will wind
itself up" and leaves the task running to completion unrecorded. A stop that
ends on "that run is over" — it is also what a stop hears after a retry landed,
so `StopAlreadyFinished` sends the stop round again. Cancelling a late outcome
at once, or waiting on a lost one forever — the two are the same dead run under
a live row, so the stop waits boundedly: waiting keeps a real completion, the
bound keeps the task stoppable. Compensating from the row alone — a quick finish
and a run ended while the host was unreachable leave the same terminal row.
Counting a result read over HTTP, or one that landed after the answer was
decided, as delivered — the model has been told nothing. Counting a failed
launch as an attempt — a shutdown would spend the ceiling on runs that never
existed. Checking the attempt ceiling in the caller — two processes could both
claim the last attempt. Sweeping orphans after requests are accepted —
`FailOrphans` would declare a just-claimed retry's fresh run dead. Escaping only
the line delimiter in a notification — a crafted result could re-aim the task id
and status on its own line. A second lifecycle beside tasks for step sequences
and loops, or a fifth model-facing verb — two tools that both mean "start
background work" are the tool-choice errors a small model makes. A precomputed
`retryable` boolean — it lags a round trip.

**Cost accepted.** One stop chases at most one retry. Two processes sharing
one store keep the sweep-vs-retry race. A durable host's debt-row guarantees
cannot live on the interface (an in-memory store has no debt), so they are
spec text.

Rules: spec §2.13

### 5.55 Fan-out buffers per subscriber, not per channel

Decided with `Fanout[T]`.

**Decision.** Fan-out is a requirement, not an optimization — measured: a slow
consumer couples to the producer under `iter.Seq2`, and under a buffered channel
too once it fills — so each subscriber gets its own buffer, and a drop is always
announced, since a consumer cannot otherwise tell a timeline missing content
from one that never had it.

**Rejected.** Dropping silently — corrupts the consumer's view undetectably.
Disconnecting the slow subscriber — turns a recoverable hiccup into a visible
failure. A single shared buffer — it couples every consumer to the slowest,
the measured `chan(64)` case. Delivering a timeline reset on the next publish
— the stream a stale cursor lands on has often already ended, and a gap
waiting for a delivery that never comes leaves the consumer in the silence it
exists to break. Reporting that reset `AtEnd` — it tells a consumer to stop
reading a run that is still going. `Close` dropping an accepted publish — the
item has a sequence number and sits in replay with no gap to report it.

**Cost accepted.** Memory per subscriber. The zero-value item beside an
`AtEnd` gap, which a forwarding consumer must skip (a nil pointer, for a
stream of pointers).

Rules: spec §2.11

---

### 5.56 Compaction's unit of work is a group, not an entry

Decided 2026-08 with `agents/compaction`.

**Decision.** A function call and its output belong together (the API rejects
one without the other), as do a reasoning block and the tool call it precedes;
entries are grouped first and a strategy only ever includes or excludes whole
groups, so cutting through a pair is not a mistake a strategy can make.

**Rejected.** Per-entry strategies with a pairing check afterwards: every
strategy re-implements the check, and one forgets.

**Cost accepted.** A group is the smallest thing a pass can drop, so a large
tool output is kept or dropped with its call.

Rules: spec §2.5f.

### 5.57 Delivery of a background result is a debt, not a call

Decided 2026-08 with the workbench's task plane (invariant 32).

**Decision.** A task finishes while its parent session may be busy, paused on
a human decision, or gone with the process, so "session S is owed a turn
carrying P" is a durable row written with the task's terminal status; the SDK
keeps no debt of its own, because when a session may be interrupted is host policy.

**Rejected.** A callback at completion time — it lands mid-run or on a paused
session, or never, after a crash. Draining per debt — three turns for three
results; one drain pays every debt through the agent that ASKED. Waking for a
task a person stopped — it owes nothing; one a shutdown ended is left for the
restart sweep, which fails it with its debt.

**Cost accepted.** A result waits for the next turn boundary; `FailOrphans`
must run before requests are accepted, and the drain after handlers are wired.

Rules: workbench invariant 32.

### 5.58 A model-authored workflow lands only through an approved save, by name

Decided 2026-08 with `save_workflow` (invariant 39).

**Decision.** Authoring is a WRITE to configuration, so the tool carries
`NeedsApproval` itself and its predicate runs the same resolve the write does
— an unsaveable proposal executes at once into a refusal the model reads, and
only a store fault still asks a person; the model addresses workflows by NAME,
the server owns ids.

**Rejected.** A schema on every agent's every request — the pair is per-agent
opt-in and chat-only, since a background run has nobody to approve.
Model-chosen ids — the server reuses a kept step's id on update, so a retry in
flight keeps naming the same step; same name means the same workflow, so a
save is an upsert. Letting the model switch the gate off. Showing the card as
the model spelled the proposal — it shows what will be stored.

**Cost accepted.** A proposal that needs a person costs a pause even when the
change is trivial. Whether model authoring stays is decided on a signal the
maintainer can see — an issue, or the maintainer's own instance — never an
audit-log count a self-hosted install never reports.

Rules: workbench invariant 39; authorization per §5.29.

### 5.59 A schema change recreates the database

Decided 2026-07-13 (invariant 25).

**Decision.** No migrations: a structural change means dropping and recreating
the database, a dev-tool stance taken deliberately. The stance is honest only
if a mismatch is loud, which is what the startup zero-row probe buys.

**Rejected.** `ALTER TABLE` migrations — a second schema language to keep
correct for a store that is rebuilt anyway.

**Cost accepted.** Production use needs this decision reversed first — before
team mode is promoted for production, or before multi-instance work starts.
Every release that changes the layout says so in its release body.

Rules: workbench invariant 25.

### 5.60 The budget rides on the input, not the instructions

Decided 2026-09 with `ContextBudget`.

**Decision.** The figure the model gets about its own window is appended as
the last input item of every call, a system text item never written to the
session; every call carries the current number.

**Rejected.** Putting it in the instructions: the number changes every call,
and an instructions prefix that changes defeats prompt caching for the whole
conversation. Threshold reminders at 25/50/75%, a shape Codex added and
dropped within ten days (June 2026) for prompt churn: one input can jump
across a mark unnoticed, and a current figure is what the model actually
reasons with. Persisting it as an entry: it describes the moment it was sent,
replays wrong later, and inflates the history it measures.

**Cost accepted.** Roughly two dozen tokens per call. Before the run's first
call the figure is the host's, so a run right after a manual compaction reports
the pre-fold number once. Dropping the previous notice makes a prefix-binding
backend (Anthropic) discard its reasoning, so the workbench sends such an agent
none (invariant 83).

Rules: spec §2.5i

### 5.61 Retrieval over summary

Decided 2026-09 with `agents/history`.

**Decision.** The model gets two read-only tools over its own session's log,
search and read, so a compaction pass may fold freely: what it folded is one
call away. The log was already kept whole for fork and replay; the tools are a
read on that property, not a second store.

**Rejected.** A summary that must carry everything: it grows toward what it
replaced, and a detail it dropped is gone. Ranked or indexed search: a session
is one conversation, a literal scan is deterministic, and an index is a second
thing to keep consistent. Deferring the tools until a result names them
(§2.7i): after a reset nothing would. Codex's "never disclose" framing: the
transcript shows the same history, and a tool the model must hide is one the
person cannot debug.

**Cost accepted.** About a thousand tokens of tool schema per call while the
tools are on. A SQL storage scans the session's bodies for a search; a
session in the hundreds of megabytes answers in about a second.

Rules: spec §2.5i

### 5.62 Memory is one store with scopes

Decided 2026-09 with `agents/memory` and the rebuilt memories table.

**Decision.** One memories table keyed by (scope_kind, scope_id, gen, key) —
global, agent and session scopes — with the rules per kind in one Go table
(`store.MemoryPolicies`); the model writes session memory freely and proposes
agent memory through the approval gate save_workflow established (§5.58), under
the agent's edit rule (§5.29).

**Rejected.** A separate session_notes table: two concepts for one kind of
thing, a promotion from session to agent scope crossing tables, and a second
tool family later. Notes as custom session entries: the compaction pass would
fold them and the timeline read would carry them. Notes as sandbox files: a
sandbox retires on a content change, and not every session has one. Model
writes to global memory: they reach every user's every agent; the policy
table has the row for it when wanted.

**Cost accepted.** The memories API is breaking (`scope_kind` and `scope_id`
replace `agent_config_id`) and the table is rebuilt. One table carries three
lifecycles — a session's rows following its fork and delete, an agent's its
delete, global's the database's — and the policy table is what keeps them apart.

Rules: spec §2.5i; workbench invariant 64.

### 5.63 A reset is a checkpoint with nothing to say

Decided 2026-09 with `new_context` and the reset compaction mode.

**Decision.** A context reset is the compaction checkpoint the log already
has, with everything but the newest user message in `ExcludedIDs` and the
model's own session memory in the summary slot; the model asks through
`new_context`, the run grants it at the save point, on the persisted log, and summary stays the default mode.

**Rejected.** Resetting mid-turn, when the tool runs: the turn's items are
not yet persisted, so a call could lose its output and the pairing rule with
it. A second projection path for the carried memory: the summary slot already
renders up front as a system message. Reset as the default: a provider not
trained to keep notes loses the task on the first fold. A save-point
compaction pass for self-compacting storages, so a run could reset itself when
the threshold trips mid-run: it changes the point contract of §2.5f for every
such storage; the budget notice and `new_context` cover the case (invariant 83).

**Cost accepted.** Two booleans on `RunState` (a schema minor), so a request
made in a turn that pauses is performed on resume. A reset folds the turn's own
tool calls, `new_context` included — which is why a fresh context refuses
another reset until the model has done some work (the kept "reset now" message
would otherwise be obeyed in every new window) and why the checkpoint's first
line says who reset.

Rules: spec §2.5i; workbench invariant 65.

### 5.64 Login admission is by verified email, whatever the provider

Decided 2026-09-08.

**Decision.** `--allowed-domains`, `--allowed-emails` and `--bootstrap-admin`
admit an address the provider verified, and that is the whole admission check
for every login provider; a GitHub sign-in is admitted by the account's primary
verified address.

**Rejected.** A GitHub organization allowlist — a second admission key, a
`read:org` scope on every login, one more API call, and an organization that
restricts OAuth App access answers "not a member" until an owner approves the
app. A GitHub handle allowlist — a handle is renamed at will and is nothing
the merge rule keys on.

**Cost accepted.** A team on personal GitHub accounts lists its addresses one
by one with `--allowed-emails`; a domain allowlist admits only the people who
keep an address on that domain as their primary GitHub email.

Rules: [OAuth mode](../howto/workbench-auth.md#oauth-mode).

### 5.65 An agent's override of the system prompt is total

Decided 2026-09-08.

**Decision.** `behavior.override_system_prompt` drops the `system_prompt`
setting from that agent's instructions wholesale, an agent with no text
sending none; the other layers keep their own switches, the harness's mode
layers stay, and a handoff target decides for itself.

**Rejected.** Falling back to the global prompt when the agent's text is
empty — the empty case is the point: an agent meant to run on nothing but its
tools has no other way to say so, and "empty means inherit" leaves it
inexpressible. A per-agent copy of the global text — a second home that drifts.

**Cost accepted.** An overriding agent with nothing written runs with no
system prompt at all; the switch's caption says so.

Rules: [invariant 67](workbench-invariants.md).

### 5.66 A retired instance whose container a successor adopted detaches

Decided 2026-09-11 (workbench invariant 27).

**Decision.** The docker adoption fingerprint (§5.19) covers only what a
container IS, so a content change outside it has the successor generation adopt
the SAME running container; a retired instance whose successor occupies the
cache releases only its connection (`sandbox.Detacher`), stopping and removing
nothing.

**Rejected.** Widening the fingerprint to the whole content — every unrelated
edit would replace the container and discard what was installed into it.
Stopping anyway — the successor's commands and shells die mid-flight and its
next call cold-starts the container it was already using.

**Cost accepted.** A successor that replaced rather than adopted sees the old
handle go stale harmlessly. Once a successor exists, only its own idle timer or
stop ends the container; a deferred user Stop that new work overtook is
superseded the same way.

Rules: workbench invariant 27; [spec
§2.7p](../reference/spec.md#27p-stop-keeps-the-filesystem-and-promises-nothing-else).

### 5.67 A list is an array on the wire and JSON text in the column

Decided 2026-09-11 (workbench invariant 1).

**Decision.** An agent's four lists are `[]string` on the REST API
(`store.StringList`), typed in OpenAPI and the generated client and refused at
bind when not arrays; the column stays `text`, the type's Valuer/Scanner writing
the JSON array, so a row written before the change reads unchanged and no schema
moves.

**Rejected.** Strings holding JSON — the shape the review found: the
OpenAPI type is `string`, the generated client is untyped, a typo in a name
surfaces at run time, and every client parses and re-serializes the field.
A relational join table per list — four tables for four lists of ids, a
schema change for what is a column's encoding, and a read that stitches rows
back into the order the operator chose.

**Cost accepted.** A breaking wire change: a client that sent the string form
gets `400`. `skills: null` appears in responses, the honest spelling of "not
customized".

Rules: [invariant 1](workbench-invariants.md);
[protocol.md, Agents](../reference/protocol.md#agents--apiv1agents).

### 5.68 A newer message abandons a paused run

Decided 2026-09-11 (workbench invariant 19).

**Decision.** A chat run paused for tool approval ends when the person sends a
new message or cancels it: the `pending_approvals` row is deleted (the claim a
racing decision loses), the calls it waited on persist as `tool_call`
annotations marked `not_run`, and the hub ends the record with `run.cancelled
{reason}`.

**Rejected.** Refusing the send (`409`) — the composer sat locked on a
question the person had moved past. Letting both stand — the later approval
resumed the old `RunState` and its answer landed after the newer turn, out
of order and out of context. Persisting the pending calls as items — an
abandoned call must not enter the model's history.

**Cost accepted.** A newer message discards a pause by design; the cards say
so. A stale hub record on a restarted server publishes nothing, so the client
resolves the cards from the newer run's `run.started` as well as the live
event and the stored marker. A background task's paused run is its task's to
stop and is left alone.

Rules: [invariant 19](workbench-invariants.md);
[protocol.md, Approvals](../reference/protocol.md#approvals--apiv1approvals).

### 5.69 A fallback entry names a provider

Decided 2026-09-11 (workbench invariant 9).

**Decision.** `resilience.fallback_models` is a typed array of
`{provider_id, model}`: the entry runs on the provider row it names, under the
primary's reference rule (§5.29), and carries no credential of its own.

**Rejected.** Keeping the inline key with mask round-tripping — the one
place a model key is entered was the provider (§5.30), and the agent form
asking for a raw key beside it contradicted that in the UI and in the
handler's second masking path. Refusing legacy rows outright — an agent
that ran yesterday must read and run today: the decode drops the stored key
and the build resolves the endpoint to a provider the agent may reference,
with a warning, or fails loudly.

**Cost accepted.** A breaking wire change: the entry shape and the field's
type. A legacy entry whose endpoint has no provider row fails the run until
the operator adds one; its stored key is inert until the next save rewrites
the field.

Rules: [invariant 9](workbench-invariants.md);
[protocol.md, Agents](../reference/protocol.md#agents--apiv1agents).

### 5.70 An E2B port is an address to copy, not a proxy

Decided 2026-09-14 (workbench invariant 53).

**Decision.** On a service speaking the E2B API every sandbox port is already
public at `<port>-<sandbox id>.<domain>`, so the workbench shows that address in
a dialog to copy; nothing is proxied, granted or published, and the row declares
it through `supports.public_host` — docker, whose ports are not public, declares
nothing.

**Rejected.** A port input in the menu — the port is the server's inside the
sandbox, which the person knows and the workbench does not. Reviving the port
preview (§5.35) for e2b — its cost was the gateway, which this needs none of.
Persisting the domain beside `instance_ref` — a schema column for a fact one
GET returns.

**Cost accepted.** One control-plane GET per open. Reachability is the
service's policy — a token gate, or a gateway answering every response with
`Content-Disposition: attachment` (Bailian's does), which leaves the URL to
`curl` and `fetch` and takes a browser page off the table.

Rules: [invariant 53](workbench-invariants.md);
[protocol.md, Projects](../reference/protocol.md#projects--apiv1projects).

### 5.71 A running record is confirmed through the daemon

Decided 2026-09-14; verified against Bailian.

**Decision.** `Status` trusts a `paused` record and a 404 and confirms a
`running` one through the daemon's `/health`, E2B's own SDK's definition of
"is running": Bailian's record says `running` for a paused sandbox — after its
own `pause` returned 204, and past the `endAt` it auto-paused at — while its
gateway tells the truth.

**Rejected.** Trusting the record — the workbench's menu offered "Stop
sandbox" on a sandbox it had just stopped. The `?state=` list filter — a scan
of every sandbox on the account, on a service that drops the metadata a
filter would narrow it by. `endAt` — an explicit pause leaves it in the
future. Remembering the pause on the client — the workbench reads through a
fresh client, and a pause from the console or a timeout is nobody's memory.

**Cost accepted.** One data-plane round trip per status read of a running
sandbox. A transient 5xx reads as stopped, whose remedy — Start — is a
`connect` that only extends a running sandbox's lease.

Rules: spec §2.7u.

### 5.72 spawn_task chooses from the handoff graph

Decided 2026-09-14.

**Decision.** `agent_name` is resolved against `Agent.Handoffs` before the
host's Resolver sees it: the model already has those names from its
`transfer_to_*` tools, so the set needs no second listing to stay in step, and
one declaration names an agent's collaborators for both shapes of delegation.

**Rejected.** Resolving any agent the host knows: the model cannot discover
the names, and a guessed one either fails or lands on an agent nobody wired to
the caller. A per-agent "spawnable" list: a second registry beside handoffs,
drifting from it. An enum in the schema: the SDK tool is built before it knows
its agent, and strict-mode schemas are reflected from the struct.

**Cost accepted.** An agent spawnable in the background is also a handoff
target, with a transfer tool the model may pick instead. A host with a flat
catalog writes its own spawn tool from the public parts.

Rules: spec §2.13; [invariant 75](workbench-invariants.md)

### 5.73 An echoed assistant message carries no logprobs

Decided 2026-09-22.

**Decision.** Logprobs annotate one response's tokens; as input they carry
nothing the model reads. Observed on a gateway fronting two upstreams: one
emits `"logprobs": []` on every message, the other rejects any input carrying
the key, so a conversation that touched both died on the next turn.

**Rejected.** Stripping in the OpenAI adapter alone: the canonical format is
provider-agnostic and a stored session would still carry the field. A setting:
nothing consumes an echoed logprobs, so there is no case that keeps it.

**Cost accepted.** One JSON pass per assistant message at echo and at history
load. A caller that set `TopLogprobs` still reads them from the response.

Rules: spec §2.1b

### 5.74 An exact approval decision outranks a standing one

Decided 2026-10.

**Decision.** A decision recorded for one call wins over an "always" decision
for its tool, whichever was recorded first; an "always" decision made through
a call replaces that call's own earlier one.

**Rejected.** Standing decisions first: "always approve" on one call ran a
second call the person had just rejected in the same paused turn. Bumping the
RunState schema for the new order: §5.18 makes a reinterpreting bump raise the
decode floor, and a host that discards undecodable states would drop every
approval pending across the upgrade.

**Cost accepted.** An "always reject" no longer stops a call approved by
itself. The schema version is unchanged, so an older build resuming a state
this one wrote resolves it by the old order.

Rules: spec §2.7

### 5.75 Work a trigger started does not run on standing command trust

Decided 2026-10-02 (workbench invariant 84).

**Decision.** A trigger's agent turn, the tasks it spawns, a trigger-started
workflow's steps and the wake-ups that report any of them each run on their
own grants, and the grant is the session's too.

**Rejected.** Reading the session's trust again once a person answered:
"approve once" then ran every later command on an older `all`. Asking for
every command with the scope ignored: the card's own buttons did nothing.
Withholding the wake-ups alone: the turn and its tasks had already run a
webhook's text under the person's trust. A per-trigger `allow_tools`: pointing
the trigger at an agent with fewer tools narrows it already.

**Cost accepted.** An unattended trigger turn that reaches a gated command
waits for a person, and every run of the chain — a retry, the next step, the
next fire — asks anew. A run's grants live in memory like the trust they stand
in for: a turn resumed after a restart reads the session's, emptied by it.

Rules: [invariant 84](workbench-invariants.md)

### 5.76 Anthropic effort maps to adaptive thinking; a thinking budget is an opt-in

Decided 2026-10-03.

**Decision.** The Anthropic adapter sends `Reasoning.Effort` as
`thinking: {type: adaptive}` plus `output_config.effort`, the form current
Claude models take; `Provider.WithBudgetThinking(true)` sends it as
`budget_tokens` for the models that predate it. Unset sends nothing.

**Rejected.** Budgets as the default: current models answer `budget_tokens`
with a 400. A per-model capability table that picks the form: scope §1.2.
Falling back on a remote 400 by matching its text. Reading `none` as "send no
thinking": some models think whatever the request says, so it is refused by
name. A flat default `max_tokens` for the adaptive path: thinking spends from
it, so the default keeps growing with the effort.

**Cost accepted.** A caller on Claude Haiku 4.5 or older who sets an effort
must opt into the budget; `minimal` reads as `low`; past `high` the default
`max_tokens` stops growing and the caller sets it. Only the budget path keeps the sampling and
forced-tool-choice prechecks: what adaptive thinking refuses is the API's to say.

Rules: [models how-to](../howto/models.md#anthropic-backend-defaults)

### 5.77 A bound reasoning block is dropped, not fatal

Decided 2026-10-03.

**Decision.** A request that already carries a thinking object also carries
`thinking.block_binding.prefix_mismatch_behavior: "drop_block"` and its beta
header, so the API drops a replayed block whose prefix changed and the adapter
reports each drop.

**Rejected.** Stripping blocks client-side from the middle of the history: a
400, and reasoning lost on every request. Leaving the default `error`: a host
that edits its prefix (a handoff, a re-rendered instruction layer) fails the
whole run on an account that enforces the check. Adding a thinking object to
carry the field: it turns thinking on for models that would not have thought.

**Cost accepted.** An account created before enforcement is opted into the
check: a mismatched block it used to pass is now dropped. A request with no
effort set carries no thinking object, so it gets no such protection. Every
thinking request carries a beta header, which `WithThinkingBinding(false)`
removes for an endpoint that rejects it.

Rules: spec §2.15

### 5.78 Injected input passes the input guardrails before it is recorded

Decided 2026-10-03.

**Decision.** A steer, next-turn input or follow-up is run through the input
guardrails where the run takes it, always to completion, whatever `Blocking`
says: a Replace swaps it for its message, a trip fails the run and consumes it.

**Rejected.** Screening the first turn only: a steer walked around the content
filter. Racing the next model call: the model has then seen what was refused.
Returning refused input to the queue: screening it again only trips again, and
a host that redelivers loops.

**Cost accepted.** Every injection waits for its guardrails. A tripped
injection fails the whole run, as a tripped first input does; an answer the
run reached before a refused follow-up is saved first.

Rules: spec §2.6, §2.11b

### 5.79 A refused final turn keeps the record that its tools ran

Decided 2026-10-03.

**Decision.** When `OnEnd` or an output guardrail fails a turn
`ToolResult.Terminate` ended, the turn is saved whole with each tool output
replaced by a fixed notice.

**Rejected.** Saving nothing: the next run in the session cannot tell the tool
ran and may repeat its side effect. Saving the real output: what the guardrail
refused enters the session. Saving the calls without the turn's reasoning and
message: a provider refuses a replayed call cut from the reasoning it came
with.

**Cost accepted.** The session holds a call whose result the model never
sees, and the stream showed an output the store does not hold; the notice is
fixed English text. A turn `ShouldStopAfterTurn` ended is out of reach: its
save point wrote the real outputs before the refusal.

Rules: spec §2.5

### 5.80 A trigger's payload is framed as data, not appended to the brief

Decided 2026-10-03 (workbench invariant 85).

**Decision.** A webhook's body follows the author's brief inside an
`<external source trigger>` block whose closing tag the payload cannot write,
then one fixed line saying the block is not the person's request; a task
notification's closing line says the same of its reports.

**Rejected.** `Payload:` and the raw body: a third party's text read as the
author's own. A developer- or system-role entry for it: more authority, not
less, and a wake-up needs a user turn. A general envelope for every non-human
input: one consumer today.

**Cost accepted.** Delimiting is mitigation: a model that follows the payload
anyway still calls every tool it is allowed. What bounds that is the approval
gate and the trust a trigger's work runs on (§5.75), not the frame.

Rules: [invariant 85](workbench-invariants.md)

### 5.81 An injected input is announced by a narrow event until run.entry ships

Decided 2026-10-03.

**Decision.** When a run reads an input queued on it, the workbench publishes
`run.injected {run_id, input, index}` and the live view splits its turn there,
as a reload does at the stored user entry; the composer queues over REST and
drops its own queued bubble by the text the event names.

**Rejected.** Shipping `run.entry` first: half the streaming reducer is
rewritten, and steering waits behind it. Reloading the page when an input is
read: the timeline flickers and races the live tail. A `client_msg_id` on the
event: the SDK reads one queue in arrival order but each kind at its own
point, and an id only this tab knows says nothing to the others.

**Cost accepted.** One more event, absorbed when `run.entry` ships. Two queued
messages with the same text are told apart by order alone. An input a
guardrail replaced matches no queued bubble: it shows in the timeline, and the
original returns to the box when the run ends.

Rules: [invariant 16](workbench-invariants.md)

### 5.82 A checklist belongs to whoever renders it

Decided 2026-10-03.

**Decision.** The workbench owns `todo_write`: a tool of its own, on an agent's
runs when `behavior.checklist` is set, off by default, refused while the session
plans; the SDK ships no checklist, its `middleware.Todo` having lost its one
consumer here.

**Rejected.** On for every chat agent, as since 2026-08 ("when a job is worth
tracking is the model's judgement"): the judgement needs the tool listed on
every request, and the listing is the cost — about 260 tokens a request, a
preamble that an overriding agent with no text still sent, and both reference
harnesses off by default. Keeping it in the SDK: a list is rendered by a host,
and the host is who knows its statuses. Folding it into `agents/tasks`: a
checklist is one run's notes, a task outlives its run. Deleting it before the
checklist benchmark reports. Per-item updates: sending the whole list is
simpler to prompt for and impossible to desynchronize.

**Cost accepted.** A weaker or local model that needs the list has to be given
it by hand, per agent rather than per model: the project keeps no table of
model capabilities (scope §1.2). An agent that had `todo_write` loses it until
the switch is turned on. Revisit when the checklist benchmark reports.

Rules: [invariant 34](workbench-invariants.md), [invariant
67](workbench-invariants.md), [invariant 91](workbench-invariants.md)

### 5.83 A stateful request is not replayed into the dark, and an attempt has its own clock

Decided 2026-10-03.

**Decision.** A request carrying `PreviousResponseID` or `ConversationID`
appends to a chain the server keeps, so a retry after an ambiguous failure could
land the turn twice; the attempt and idle clocks are `context.WithCancelCause`
clocks carrying the policy's own error, so a timed-out attempt is retried
whatever `RetryIf` says and is never mistaken for the caller's cancellation.

**Rejected.** A `ReplaySafe` classifier on the policy: no caller needs a
different rule, and the transport shape of an error is visible without the
adapter. Treating every 5xx as unsafe for a stateful request: the server
answered, and a status is the one signal there is. `context.WithTimeoutCause`
for the attempt clock: its deadline cannot be released at the commit point,
and a long answer that started promptly would be cut. A per-call deadline
hidden in the HTTP client: it cannot tell the first byte from the last.

**Cost accepted.** A stateful request that times out fails after one attempt
even when the server never saw it. A stall after output ends the stream with
an error rather than resuming it: the Responses API resumes a stream by
sequence number, which no decorator does yet.

Rules: spec §2.16.

### 5.84 A thinking block remembers the prefix it was bound to

Decided 2026-10-03.

**Decision.** The Anthropic adapter writes a fingerprint of the request prefix
(system text and tool definitions) into each thinking block's
`encrypted_content` and on replay leaves out the newest block bound to another
prefix with every block before it: the host re-renders its instruction layer
every run, so the prefix moves mid-session as a matter of course.

**Rejected.** Storing the fingerprint on the SDK entry: a field on
`session.Entry` every provider would carry for one adapter's rule. Hashing the
whole message prefix: a block is bound to its own turn's history anyway, and
the system text and tools are what a host edits. A fingerprint on the OpenAI
side: no failure has been reproduced there.

**Cost accepted.** Blocks written before fingerprints replay no more; the
first request after the upgrade thinks from scratch. An instruction edit
costs the conversation its earlier thinking, as the API would have; §5.77's
`drop_block` remains the net under an account that enforces the binding.

Rules: spec §2.15.

### 5.85 An agent's approvals are a mode, not a checklist

Decided 2026-10-03.

**Decision.** An agent asks in one of three modes — `never`, `on_change`,
`always` — where "a change" is plan mode's own answer,
`Plan.ReadOnlySet().Admits`, so the set the mode asks about and the set
planning denies are one set; the per-tool list only ever adds a question.

**Rejected.** The 18-box checklist of built-in tool names: hard-coded, so it
missed every tool added after it, and MCP tools were a text box beside it.
Writing `always` as `approve_tools: ["*"]`: the runner's `"*"` answers for
`exec_command` too and routes it around the per-command gate. Trusting an
MCP tool's `readOnlyHint` for "a change" (§5.53). A mode that exempts
listed tools: a list that both adds and removes questions reads two ways.
Defaulting new agents to `on_change`: a run nobody is watching would pause on
its first write.

**Cost accepted.** `on_change` asks about a session memory note or a
checklist update, as plan mode refuses them. A tool's own predicate still runs
under a mode (for its error), then is overruled.

Rules: invariant 90; spec §2.12.

### 5.86 Hosted harnesses are not wrapped

Decided 2026-10-03.

**Decision.** The SDK ships no `Model` adapter or wrapper package for a hosted
harness (the OpenAI Agents API, the Codex app-server), and the server does not
speak their wire protocols.

**Rejected.** An Agents API adapter: the loop would run twice, and approvals
and guardrails would gate only the outer one. Codex app-server compatibility:
a large surface that changes often; a terminal client is cheaper written
natively.

**Cost accepted.** Someone who wants a hosted feature uses the hosted service
directly, outside this project's traces, replay and approvals.

Rules: [scope §1.2](scope.md#12-non-goals).

### 5.87 The session conformance suites are public

Decided 2026-10-03.

**Decision.** `agents/session/sessiontest` carries `StorageConformance` and
`RepoConformance`, the suites the SDK's own session backends pass, so a
backend written outside the repository runs the same checks from its tests; the
fake model and the run-level assertion helpers stay in `internal/agentstest`
(§5.23).

**Rejected.** Leaving the suites internal: the contract in spec §2.5e2 is then
checked only for the backends in this repository, and the Redis or encrypted
store [scope §3](scope.md#3-capabilities-deliberately-not-provided) points
people at is written against prose. A conformance check for atomic batch
appends: no backend can prove the negative from the outside, so it stays a
documented contract.

**Cost accepted.** One more public package to keep compatible; its surface is
two functions and one struct.

Rules: spec §2.5e2.

### 5.88 Deferred tools reach the wire as a shorter list

Decided 2026-10-03.

**Decision.** A deferred tool is withheld by the runner, and no adapter
renders a provider-native deferral; when a producer appears, the runner's own
retrieval matches by literal name, as §5.61 does, never by a model-side search.

**Rejected.** Adapter-rendered deferral: the two providers' mechanics differ
and both are still moving, and each disclosure is a prefix edit on either
path until a native form is adopted. Turning the feature off for Anthropic
alone: the filter is backend-agnostic, and the prefix cost is the same one
§5.77 and §5.84 already absorb. Removing the surface now: it is the one
mechanism spec §2.7i specifies, and it is tested.

**Cost accepted.** Every disclosure changes the tools array: a prompt-cache
miss on OpenAI, dropped reasoning on a prefix-binding backend. Nothing in this
repository produces a deferred tool; absent a producer or a settled native
deferral by the next breaking minor, the surface goes under the zero-consumer rule.

Rules: spec §2.7i.
