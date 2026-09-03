# Design decisions

Decisions that have been discussed and settled, each with the reason it is what
it is. **Read the rationale before reopening one.** The section numbers are
permanent addresses — code comments cite them as `decisions §5.29`, so a number
is never reused or renumbered; a retired decision keeps its heading as a
tombstone.

Every entry has one shape: **Decision**, **Rejected** (each alternative and
the one reason it lost), **Cost accepted**, and — when the rules the decision
produced live elsewhere — a closing `Rules:` line naming the
[spec](../reference/spec.md) section or the
[workbench invariant](workbench-invariants.md) that holds them. What the
project deliberately does not do lives in [scope](scope.md).

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
topology up front". They solve different problems, and handoffs carry an
`InputFilter` and history folding that a graph model needs a lot of glue to
express.

**Rejected.** Graph orchestration as the multi-agent primitive — if it ever
arrives it layers *above* handoffs, serving task orchestration, not replacing
agent switching.

Rules: spec §2.4.

### 5.2 Names describe the thing, and renames are batched

Retired as a ledger: the renames it listed rode the v0.3.0 window and live in
the release notes. What survives: a name earns a rename only when it
misdescribes or breaks a Go rule, never to "look less like Python"; a rename
is a breaking change batched into a window users absorb once (§5.8), and the
next window is the openai-go v4 bump (§5.5b).

### 5.3 `Instructions` and `Prompt` both stay; both are func types

**Decision.** `Prompt` (a server-stored template with a version and variables)
is a Responses API capability, not a porting artifact; the two compose — a
stored prompt provides the base, instructions append. Both are **func types**:
`StaticInstructions` / `StaticPrompt` cover the fixed case, `WrapInstructions`
composes, and resolution (nil handling, prompt-ID validation) is the runner's
job behind unexported entry points, not API surface.

**Rejected.** Single-method interfaces with `...Func` adapters — their only
implementations were unexported types in this package, a plug point nothing
ever plugged into; a func type is the same capability assigned directly. The
same rule collapsed `tasks.AgentResolver`, `Launcher`, `Stopper` and
`WakeGuard`; `tasks.Store` (multi-method) keeps being an interface. A
single-method injection point is a func type unless a second method is
already in sight.

### 5.4 A tool is a struct, not an interface

**Decision.** `*Tool` is the tool type. There is no `Tool` interface, which is
how the "no hosted tools" line ([scope §1.2](scope.md#12-non-goals)) is
enforced: a provider-hosted tool has nowhere to be introduced, because there
is nothing to implement. Behavior stays open because the fields are exported
and a variant is a copy.

**Rejected.** A sealed interface with an unexported marker method — the seal
closed the kind just as well, but it invited a wrapper hierarchy to carry
optional behavior, and that hierarchy needed a lookup protocol to be usable.

Rules: spec §2.7c.

### 5.5 Internal item types are Responses wire types

**Decision.** Zero conversion, zero information loss — reasoning ids,
`encrypted_content` and strict schemas all survive round-trips.

**Cost accepted.** Non-LLM entries need a `session.Entry` wrapper to have
somewhere to live, and the coupling in §5.5b.

### 5.5b The wire types couple our compatibility to openai-go's

**Decision.** `InputItem` and friends are **type aliases of `openai-go/v3`
union types**, and they appear in nearly every exported signature. A
major-version bump of openai-go (v3→v4) is therefore a breaking change of this
SDK's entire API surface, whatever else it contains. The major version is
pinned in `go.mod`; nothing forces a bump on users until one is taken
deliberately, and **when it comes it is the merge window** for every other
API-surface change on the shelf, so users absorb one migration (§5.8), not
two.

**Rejected.** Wrapping the wire types behind our own structs — it costs the
round-trip fidelity §5.5 exists for, plus a conversion layer that must chase
every Responses API addition forever.

### 5.6 Background work runs in-process, not in isolated processes

**Decision.** Background sub-agents ("tasks") run as nested runs inside the
same process, each with its own hidden session, reporting back by injecting a
notification into the parent session.

**Rejected.** One supervised OS process per session over a line protocol — it
buys crash isolation and independent working directories at the cost of IPC,
serialization and a second lifecycle; nested runs already give independent
sessions and configuration, and the isolation is not worth the machinery at
this scale.

Rules: spec §2.13.

### 5.6b Tracing stays vendor-neutral; OTel export is the consumer's job

**Decision.** The core `tracing` package has no dependencies: a span is a
flat record with string ids and a `Data` map, and export is a consumer-side
`tracing.Processor`. Two rules keep the record portable — OTel id widths, and
one root span per agent rather than per trace.

**Rejected.** Emitting OTel spans from the core — a heavy, fast-moving
dependency in every consumer's build for a feature most do not use. An
in-repo exporter submodule — zero consumers, since the workbench reads spans
through its own store (§5.23).

**Cost accepted.** An exporter that groups by trace must carry workflow
metadata across an N-handoff run's N+1 parentless spans itself.

Rules: [Tracing](../howto/tracing.md).

### 5.7 A submodule exists only to keep a heavy dependency out of the core

**Decision.** The repository is a Go workspace with a root module (the SDK)
plus submodules, and the **only** reason to split something into its own
module is that it would otherwise pull a heavy dependency into the core. Test
helpers, small utilities and anything dependency-free stay in root regardless
of how self-contained they are.

**Rejected.** Splitting by cohesion — `mcp` is a module because
`modelcontextprotocol/go-sdk` brings a raft of indirect requirements, and for
no other reason; the core holds servers through the `agents.MCPServer`
inversion, so the split cost one `go.mod` and moved no import path.

Rules: [Architecture](architecture.md#module-boundaries).

### 5.8 Public API compatibility begins at v1.0.0

**Decision.** A minor release before v1.0.0 may break exported identifiers.
Each break is recorded in the release notes with the old spelling beside the
new, and breaks are batched into as few releases as the work allows, so a
user absorbs one migration rather than a drip.

**Rejected.** A deprecation cycle before v1.0.0 — it was promised once and
not kept through the structural collapses, and a rule nobody follows teaches
the reader that this document describes intentions rather than behavior. The
cycle begins when the API stops finding its shape.

### 5.9 A parent-linked checkpoint chain for execution state is declined

**Decision.** No second history structure beside the session tree. The tree
IS the parent chain (spec §2.5d): "re-run from message X" and "same history,
different options from turn N" are branches from any leaf. `RunState`
serializes the one state that cannot be rebuilt — the mid-turn pause awaiting
approval — and per-turn persistence bounds crash loss to the in-flight turn,
with repair (spec §2.5h) making the session loadable again.

**Rejected.** A parent-linked checkpoint per superstep, browsable as a tree
(agent-framework-go's design) — it needs that structure because its session
is a key-value bag with no other history; here the net gain is deterministic
replay and byte-exact "resume turn N as it was", which does not justify a
second structure with its own consistency rules against the tree.

**Cost accepted.** No time-travel debugger. Revisit only with a concrete
replay need, and then on three terms: a checkpoint is a session ENTRY kind
(a trimmed `RunState`, projected to nothing) so the tree stays the only
history; a deterministic execution mode comes first, because replaying a
nondeterministic run replays into different behavior; and the payload is
trimmed — `RunState` carries every raw response, and a per-turn copy grows
quadratically.

### 5.10 Non-Responses backends adapt at the model boundary

**Decision.** The canonical item and event format stays the Responses wire
format (§5.5) even when the backend speaks something else. An adapter
translates in both directions **inside its own package** — `models/anthropic`
for the Messages API — so the runner, sessions, run state and the server never
learn a second format. `models/modelkit` (root module, dependency-free) holds
the shared halves: the input walker, item/event synthesizers that stamp
round-trippable raw JSON, the feature-rejection helper, and the conformance
suite every adapter runs. `models/anthropic` is a submodule per §5.7.

**Rejected.** A second canonical format, or a neutral abstraction both
backends map onto — a lowest-common-denominator model loses exactly the
Responses semantics (reasoning ids, encrypted content, strict schemas) the
SDK guarantees depth on. Chat Completions as the second backend — declined in
favor of a native Anthropic adapter ([scope §1.2](scope.md#12-non-goals)).

**Cost accepted.** Each adapter re-implements the translation, and the
adapter alone knows what its backend cannot express — which is why an
unsupported feature must fail loudly rather than drop silently.

Rules: spec §2.15 (the adapter contract); the Anthropic mappings and defaults
are in [Models](../howto/models.md).

### 5.11 Construction errors split by data provenance

**Decision.** A constructor whose failure can only be a programmer error
**panics**; one whose input is runtime data **returns an error**. `NewTool` and
`AgentAsTool` derive their schema from a Go type — deterministic per type, so a
failure is a bug any test surfaces immediately (the `regexp.MustCompile`
precedent), and panicking keeps them chainable inside `Agent{Tools: ...}`
literals. `NewRawTool` takes a schema that is data, so it returns
`(*Tool, error)`.

One failure is a shape rather than a bug: strict mode cannot express an `any`
field or a map with arbitrary keys at all, and `Tool.NonStrict` cannot rescue
it — it relaxes a tool that already exists, while the strict schema is built
during construction. So `NewTool` has a non-strict twin, `NewToolNonStrict`,
mirroring `OutputType` / `OutputTypeNonStrict`. `AgentAsTool` has none — a
recorded gap, not a decision: no caller has needed an unconstrained field in a
nested run's arguments, and until one does the way out is building the `Tool`
value directly. The normalization errors say to turn strict off *where the
schema was built*, because the same message is reached from `NewRawTool` and
`NewDynamicOutputSchema`, where the switch is elsewhere.

**Rejected.** Returning a tool that errors on every invocation, surfaced by
the runner before the first model call — it deferred a deterministic bug to
runtime and cost a field plus a runner check.

### 5.12 One user-context entry point

**Decision.** `RunOptions.Context` is the only way user data enters a run;
every run wraps it in a fresh `RunContext`. Nested runs share the parent's
`Context` value with fresh accumulators, and cross-run usage totals are sums
over each `RunResult.Usage`.

**Rejected.** A field to inject a pre-built `RunContext` — two fields for one
concept, and a run owning its `RunContext` outright is what the guarantee "a
run's accumulators start empty" rests on.

### 5.13 AgentToolConfig configures the tool, ModifyRunOptions the run

**Decision.** `AgentToolConfig` holds only what has no `RunOptions`
counterpart: the tool's name, description, visibility, approval gate, error
rendering, output extraction, streaming callback and input rendering.
Everything about the nested run itself — session, turn budget, conversation,
model, guardrails — goes through the single `ModifyRunOptions` channel.

**Rejected.** Mirror fields (`MaxTurns`, `Session`, `ConversationID`) — each
was a second spelling of a `RunOptions` field, and the escape hatch's
existence proved the dedicated-field approach could never be complete.

**Cost accepted.** A `ConversationID` set via `ModifyRunOptions` is cleared
when a paused nested run resumes; the serialized state already carries the
conversation.

### 5.14 Sandbox file tools share exec's path view

**Decision.** The file operations resolve paths with shell semantics,
identical to `exec_command`. The isolation boundary is the sandbox, not the
working directory — exec already reaches everything on that filesystem, so
pinning the file tools inside `WorkDir` adds no protection and creates a
second path universe; the model learns real absolute paths from exec output
(`pwd`, `ls`, `git status`) and echoes them into the file tools, so one shared
view is what makes those calls work.

**Rejected.** A workdir-rooted "virtual chroot" — absolute paths got re-joined
under `WorkDir` and read as "not found". Docker's archive API (`docker cp`)
for persistent containers — it reads only the graph-driver filesystem and
cannot see a tmpfs or volume mount, so a file exec had just written under
`/tmp` read back as absent; every file operation goes through `exec` instead.

**Cost accepted.** Docker bind-mount mode is the one exception: its file
operations run on the host side of the mount, so there they are confined to
`WorkDir` via `os.Root` and translated from the in-container mount point —
`sandbox.ErrOutsideWorkDir` rather than a silent re-rooting.

The docker backend's three-way file dispatch is written out in each method: an interface over two backends never chosen dynamically would hide which one runs without removing a branch.

Rules: spec §2.7t.

### 5.15 Streaming-only backends adapt with a Model decorator

**Decision.** A backend that accepts only streaming requests — the ChatGPT
Codex backend rejects a non-streaming POST with 400 — is adapted by
`NewStreamOnlyModel` / `NewStreamOnlyProvider`: `Respond` runs the request as
an internal `StreamResponse` and assembles the final response from the
terminal event, sharing the runner's own `responseAssembler` so the two paths
cannot drift. It composes **innermost**, directly on the backend, so retry,
fallback and routing above it see a severed stream as an ordinary `Respond`
error.

**Rejected.** Forcing `"stream": true` as an HTTP middleware — it hands an SSE
body to a caller that parses a JSON response; the request shape and the
response parser must switch together, which only the model boundary sees.

**Cost accepted.** The assembled response carries no `RequestID`, and a
length-truncated `response.incomplete` counts as arrived, not failed — the
same as the runner's streaming path.

### 5.16 A severed stream retries only before output, with the preamble held back

**Decision.** `NewRetryModel` and `NewFallbackModel` may replace a broken
streaming attempt only while nothing the model **generated** has been
delivered. Lifecycle preamble and terminal-failure events carry nothing
generated, so they are buffered rather than delivered: an abandoned attempt's
pending events are dropped and the consumer sees exactly one coherent
response. A stream that ends cleanly without its terminal event is an
adapter's truncation error, so the transport classification can retry it.

**Rejected.** Retrying after output — a delivered event commits the consumer
to a response a second attempt then duplicates. Committing on the preamble —
`response.created` arrives the moment the connection opens, which would make
every severed stream unretryable. Treating a clean EOF at an event boundary
as a finish — accurate but unretryable; the runner keeps that check only as
the last line of defense. Failing a call on a transport error AFTER the
terminal event — the response is complete, and a valid result would be thrown
away over a connection with nothing left to say.

**Cost accepted.** A `response.incomplete` commits (a length-truncated
response is output that arrived), so a retry never rescues one; and every
decorator that saw a post-commit error records it, so a nested chain accounts
for one break once per layer.

Rules: spec §2.7e.

---

### 5.17 The session layer is its own package

**Decision.** `agents/session` owns stored history: entries, storage, the
semantics struct, projection, the tree, forking, recovery and the wire codec.
The runner imports session; session never imports the runner — its one upward
need, building an entry from a live `RunItem`, stays in agents as
`EntryFromRunItem`. Names inside drop their `Session` prefix; `session.Session`
keeps the stutter the way `context.Context` does, because the concept IS the
package.

The value types both layers share — `Source`, `ItemDisplay`, `RequestUsage`,
`Diagnostic`, `ErrorCode` — live in session (entries persist them) and are
**aliased** in agents under the same names: an alias is transparent only while
nothing spells the type out, and a renamed alias makes the compile error, the
godoc and the reflected name disagree with the code. `ErrorCode`'s vocabulary
sits in session; its derivation (`CodeOf`, `Classify`) stays in agents with
the error types it reads.

**Rejected.** Aliasing session-only names into agents — code that works with
stored history imports the package that owns it.

Rules: spec §2.5c.

### 5.18 A RunState decodes across a version window, and the window is earned

**Decision.** `RunStateFromJSON` accepts the same schema major from
`runStateOldestDecodableMinor` up to `RunStateSchemaVersion`. A pause waits on
a human, the process may be redeployed while they decide, and refusing the
state afterwards strands the run for a reason the user had no part in. The
field-by-field fallbacks the decoder carries (a zero `MaxTurns`, a nil
`UsagePending`, an absent cursor) are what make an older minor readable.

The window is earned, not retroactive: **a minor may only ADD fields**, and a
bump that replaces or reinterprets one raises the floor to itself, because
such a state decodes *successfully* with its old fields dropped — worse than
a refusal, since the caller is told the resume is faithful. The floor is 4:
`"1.3"` was stamped on two incompatible payloads (before and after the
guardrail-result keys collapsed), and accepting it would drop every recorded
guardrail result from the older shape.

`RunState.Extra` (1.6) is host-owned state riding the pause, marshalled
verbatim and never read: a build-time agent transform (`middleware.Plan`)
returns fresh state on rebuild, so what it knew must travel with the pause or
the host invents a side channel. It covers pause→resume only; a fact that
must survive a crash mid-run needs the host's own durable write.

**Rejected.** Strict version equality — the fallbacks were cost with no
payer, and an equality gate destroys states an additive bump resumes fine.

Rules: spec §2.1 (`RunStateVersionSupported` is the consumer's gate).

### 5.19 A named container is adopted only against a configuration fingerprint

Revised 2026-08-24 with derived container names (§5.28).

**Decision.** A persistent docker sandbox with a fixed `ContainerName` may
take over a container already holding that name only when a label proves it
ours and its fingerprint — a hash of every security-relevant option, on
**effective** values — matches exactly. Ours-with-a-different-fingerprint is
ours from an older configuration and is **replaced** (removed, recreated):
with `KeepOnClose` the containers outlive the process, and every config edit
would otherwise strand its old container on the derived name forever. A
container without the label is foreign and a hard error naming the remedy.

**Rejected.** Matching on image + mount alone — a container created under a
laxer policy (network on, root, no limits) passed both checks and silently
served a config that no longer allows any of it.

Rules: spec §2.7n.

### 5.20 A shared connection is not a caller's to cancel

**Decision.** An MCP session is shared by everyone configured with that
server — several runs, their tasks, other conversations — while a run's
context belongs to one of them. A request therefore rides the connection's
context, and the caller's cancellation is honored by returning from the wait,
not by cancelling the request. The rule generalizes: **a resource shared
between runs may not be handed a single run's cancellation**.

**Rejected.** Issuing each request on the caller's context — the go-sdk's
streamable HTTP transport fails the whole CONNECTION when one request is
cancelled mid-flight (a `sync.Once` closing its failure gate), and every later
call by anyone answers "client is closing"; one person stopping one run was
observed failing five background tasks across two conversations, each blamed
on its own agent's server.

**Cost accepted.** One in-flight request outlives its caller, bounded by the
connection's lifetime and a request ceiling generous enough to fire only on a
request that is already lost.

Rules: spec §2.16.

### 5.21 A dead shared connection repairs itself, and a tool call is not repeated

**Decision.** Nothing in the go-sdk reconnects, so the connection owns its
own recovery: given `mcp.Options.Redial`, a session found dead is replaced
**in place**, so every holder of that server recovers rather than only the
runs that start afterwards. Death is noticed by watching the connection, not
by a caller tripping over it; healing is throttled; and only idempotent work
is repeated — `tools/list` is re-issued, a failed tool CALL is reported to the
model, because a dead line cannot say whether the server ran the tool, and
running a write twice is worse than reporting it once.

**Rejected.** Reconnecting without `Redial` — only the configuration's owner
can rebuild a transport (an `*exec.Cmd` is spent once; an endpoint needs its
headers, proxy and OAuth handler), so recovery is opt-in and the default
reports the failure.

Rules: spec §2.16.

### 5.21b An MCP retry waits on the transport, never on an answer

**Decision.** `MaxRetryAttempts` retries a transport failure — each attempt
reloads the session, so a connection the watcher healed carries the next try
— and never an answer the server sent (a JSON-RPC error means it understood
the request and refused it; the same bytes earn the same refusal) or a call
after `Close`. The delay is capped and jittered to match the model layer's
timing, so a server shared by many runs is not retried in lockstep. The two
bounds are what let `-1` be a real setting rather than a footgun: one attempt
per cap until the caller's context ends, with the errors that could never
succeed leaving on the first try.

**Rejected.** Sharing the model layer's `RetryPolicy` — its `DefaultRetryIf`
(retry everything but cancellation) is exactly the policy that made an
infinite MCP retry indistinguishable from a hang; one knob with two defaults
serves neither. An uncapped exponent — a one-second base sleeps half an hour
by the twelfth attempt.

Rules: spec §2.16.

### 5.22 Retry policy lives in one layer

**Decision.** `openai.NewProvider` and `anthropic.NewProvider` build their
clients with `WithMaxRetries(0)`; the SDK's one retry layer is
`NewRetryModel` — provider-agnostic, classifiable (`RetryIf`) and observable
(a span per attempt). A provider used without it performs no retries; a
caller's own `option.WithMaxRetries` is appended after the default and
re-enables the transport layer.

**Rejected.** Letting both layers retry — the official clients' default two
attempts and `NewRetryModel` compose multiplicatively, and neither can see the
other. Clamping a `Retry-After` longer than `MaxDelay` to the cap — a wait the
caller capped below what the server asked for is a signal to stop, so the
retries end with that attempt's error.


`modelkit.RetryableError` classifies as it does for these reasons: a
`DeadlineExceeded` is usually the attempt's own budget, the hung-request case
retrying exists for, and when it is the caller's context the next wait sees
`ctx.Err()` and stops anyway; an explicit `X-Should-Retry` outranks the status
because the server knows whether THIS failure is transient, and only its two
exact values carry meaning; `io.ErrUnexpectedEOF` is retryable because a
gateway severing an SSE stream arrives as a plain io error, not a `net.Error`.
`agents.DefaultRetryIf` is the coarser SDK-level default (everything but
cancellation and deadline expiry); the two are different layers.

Rules: spec §2.16.

### 5.23 Zero-consumer surface was cut to the workbench's actual needs

Retired 2026-08 as a ledger of cuts under scope §1.2's zero-consumer rule
(`tracing/otel`, `filesession`, `tools/bravesearch`, `cmd/verify`, MCP serve).
What survives: `internal/agentstest` is test infrastructure, not API — testing
against the SDK means implementing `agents.Model` — and docs are synced inside
the change that moved the code, never by a checker run afterwards.

### 5.24 The workbench has no provider routes

Decided 2026-08-24.

**Decision.** An agent names its provider (`provider_id`), full stop;
cross-provider mixing inside one run is `fallback_models`, per agent and
visible in its config. The SDK's `RouterProvider` stays as documented API for
embedders; the workbench never builds one.

**Rejected.** A global prefix→provider table (`provider_routes`) — two
selector surfaces for one decision, and the global one silently overrode the
agent's. Do not reintroduce it; a future need for shared endpoint selection
belongs on the provider rows themselves.

### 5.25 The workbench speaks one MCP transport: streamable HTTP

Decided 2026-08-24.

**Decision.** `McpServerConfig` carries no transport discriminator: `Config`
IS an `HTTPMcpConfig`. A local stdio-only server joins through a stdio→HTTP
proxy run and supervised by the operator, outside the workbench's authority.
The SDK's `mcp` module keeps its stdio transport — an embedder spawning a
subprocess in their own program is their own trust decision.

**Rejected.** Stored stdio servers — arbitrary command execution on the host
as the server's process user behind one admin write, and the blocker for ever
letting non-admins configure MCP. A sandboxed variant would be a new decision
argued here first.

### 5.26 A skill is one SKILL.md document

Decided 2026-08-24, a deliberate narrowing of the Agent Skills format.

**Decision.** A skill is the SKILL.md document alone — no bundled
`scripts/`, `references/` or `assets/`. A single document lives in the
workbench's database like every other configuration entity, and the SDK's
`skills` module shrinks to storage-free primitives: `Parse` validates a
document, `RenderIndex` renders discovery, and activation is a `read_skill`
tool the caller provides.

**Rejected.** The full format's file trees — they force skills onto a
filesystem, and a path-based reader needs an os.Root confinement the
document model does not.

**Cost accepted.** A skill that instructs the model to run bundled scripts
does not get them; references to a repo's other files dangle, and the model
follows the instructions by writing its own code in the sandbox. Import URLs
are member-supplied outbound requests with no SSRF defense — §5.29's accepted
risk. Do not add per-skill file storage back; a skill needing an artifact
inlines it or instructs the model to fetch it.

Rules: [Skills](../reference/protocol.md#skills--apiv1skills) in the wire surface.

### 5.27 The workbench's sandbox is Docker, and SSH is how a remote daemon is reached

Decided 2026-08-24; revised 2026-08-28, see §5.36.

**Decision.** A workbench sandbox is a Docker container or, since §5.34, an
E2B-compatible one. For Docker what varies is WHERE: `host` empty for the
local daemon, `ssh://user@host` for a remote one, `tcp://` for the exposed
case. SSH lives on inside `sandbox/docker` as a TRANSPORT — a pure-Go dialer
opening streamlocal channels to the remote `docker.sock` over one shared,
self-healing connection, needing only sshd with streamlocal forwarding and
socket access for the SSH user. Self-healing covers transport failures only:
a rejected channel open (a container port nothing listens on yet) arrives on a
healthy transport, and reconnecting on it would sever every terminal
multiplexed on the same client. The SDK's `sandbox.LocalSandbox` stays for
embedders and tests; the server never offers it.

**Rejected.** A `local` sandbox type — host execution behind one admin write
and one approval, and the reason a web terminal had to be special-cased off.
A generic `ssh` sandbox — a login user's full privileges, no limits, on a
machine the server merely had credentials to; an embedder wanting raw remote
exec uses x/crypto/ssh directly, since the value this repo adds is the
sandboxing SSH never provided. A remote docker CLI or a local ssh binary as
the transport — the server shells out to nothing.

**Cost accepted.** An isolation need beyond containers (VMs, gVisor) is a new
backend decision argued here first — gVisor is already reachable via
`runtime: runsc`. Do not reintroduce a host-exec or raw remote-exec type.

Rules: workbench invariant 27 (where a sandbox runs is its identity).

### 5.28 A project is the unit of working storage, and containers are per project

Decided 2026-08-24; revised 2026-08-25 (§5.29), 2026-08-28 (§5.33, §5.36)
and 2026-08-29 (deletion contracts in SQL).

**Decision.** A **project** is one user's working tree on one sandbox, named
per (owner, sandbox) and display-only — storage is keyed by id, so a rename
moves nothing, and no user-typed path ever reaches a mount. The machine
affinity is deliberate: a tree lives on one daemon, and a project that could
"move" between daemons would silently be two different sets of files. A
session's permanent binding is `project_id`; execution is always the
container's `/workspace`, which mounts the project's storage. Containers are
persistent-only, **one per project**, deterministically named so restarts
re-adopt by fingerprint (§5.19) instead of duplicating, and kept (stopped, not
removed) so installed packages survive idle. A run naming no project gets no
sandbox tools at all.

Projects are the first PERSONAL configuration entity: every member manages
their own, scoped in the handlers rather than the admin gate, and an admin
additionally MANAGES the plane (§5.29's manage-not-author line). The web
terminal follows the same line — a member opens a shell into their OWN
project's container, an admin into any: the operator's escape hatch, and a
deliberate exception to "session content is owner-only".

**Rejected.** A free-form working directory per session — it made "which tree
did that command touch?" a question with a surprising answer. Cascading a
sandbox delete onto its projects — a project delete reclaims storage, so the
cascade would destroy working trees as a side effect of removing a machine;
the sandbox delete refuses instead.

**Cost accepted.** Deletion and binding contracts settle in SQL per dialect —
single-statement guards on SQLite, parent-row locks on PostgreSQL — two
shapes to keep equivalent.

Rules: workbench invariant 27 (binding, fences, locks); the operational
surface is [Projects](../reference/protocol.md#projects--apiv1projects).

---

### 5.29 Configuration is scoped per row: private to its owner or global

Decided 2026-08-24; owner semantics revised 2026-08-25, listing order
2026-08-31.

**Decision.** The five configuration entities members compose runs from —
agent configs, providers, MCP servers, skills, workflows — carry two
independent columns: `scope ∈ {private, global}` decides **who sees** the
row, `owner_id` names **who wrote** it. The owner is permanent — stamped at
create, surviving every scope flip, changed only by an explicit transfer. A
private row is invisible to other members (404, absent from listings), so
scope is not an existence oracle; a create defaults to private.

The write matrix follows from the two columns. The author edits what they
wrote, private or published; an admin edits any global row but **not** a
member's private one — a config an admin could silently rewrite under a
member's name would blur whose credentials and instructions a run carries.
Publishing is the admin's alone, because publishing to every member is the
review point and the reviewing role does it; unpublishing is the admin's or
the author's, and the row returns to its author, who never left it. A
transfer re-validates the row's references AS THE NEW OWNER exactly as a
save does — a config that answers 204 and then fails every run is the state a
save already rejects. Names are unique per visibility context, and wherever a
name resolves it is **own-over-global**: scope, not authorship, is what "own"
means, so an author who published a name still gets a private row of it.

References split by whether they are load-bearing. `RefVisible` holds at
write time where a dangling reference breaks the holder, and a **global
holder may reference only global rows** — otherwise promoting it would publish
a config whose parts most members cannot see. The provider leg, the one that
spends a credential, settles its races in SQL and is re-checked at run time;
the advisory legs filter to the run owner's visible subset instead. Scoped
listings order by AUTHORSHIP — others' shared rows first, then one's own — on
the permanent `owner_id`, so only a transfer ever reorders a row.

**Rejected.** Admin edits on private rows — the ownership blur above. A
per-row ACL — one team, one trust boundary. A same-scope flip as a no-op — a
flip is defined FROM the other scope only, so two racing demotes cannot both
flip a row.

**Cost accepted.** A member's published row stays theirs to change after the
admin approved it. Member-supplied URLs (MCP endpoints, skill imports) get
**no private-network/SSRF defense**: the deployment model is one team, one
trust boundary, and egress control is applied outside the server.

Rules: workbench invariant 42 (the in-write re-check); the status matrix and
list ordering are in [the wire surface](../reference/protocol.md#authorization).

---

### 5.30 A credential lives on the row that spends it — no global fallback keys

Retired 2026-08-24 as a ledger of removed settings (`openai_api_key`,
`anthropic_api_key`, `brave_api_key`, `github_token`). What survives: a key
any keyless row inherits is the ambient authority §5.29 removed — a provider
row carries its own key or runs keyless, an agent naming no provider fails
its pre-flight, and settings keep the KindSecret machinery with no keys in it.

---

### 5.31 A skill's identity carries its repository; a repo publishes as one group

Decided 2026-08-25, refining §5.29 for skills alone.

**Decision.** An import lands a whole repository's `SKILL.md` files at once,
and two repositories may each ship a `review`, so **the repo is part of the
name**: the model-facing name is `<repo label>:<frontmatter name>`, and
uniqueness keys on `(source_repo, name)` within a visibility context. The
label is materialized on the row (`repo_label`) because two source URLs can
reduce to one label, and a duplicate qualified name would make `read_skill`'s
answer a coin flip. **A repo group is one scope and one owner**: scope and
ownership move per `(source_repo, owner_id)` group in one statement, all or
nothing, and every operation on a group NAMES it rather than searching for a
plausible one — otherwise an admin holding a private copy of a repository
somebody else published would refresh their own copy believing they synced
the published one. A sync's new files inherit the group's scope and owner,
and an import fetches everything first, then writes in one transaction that
re-reads the group under lock and refuses, nothing written, when its
`(owner, scope)` moved during the minutes of network.

**Rejected.** Keying uniqueness on the raw source URL — it lets the
two-URLs-one-label pair through. Merging on a transfer into an owner who
already holds a group for that repository — that is how a mixed-scope pile
forms, and the unique indexes cannot see it because they partition BY scope.
A persisted aggregate root for the group — the invariant needs one consistent
instant, not a table; the rows already carry the identity, and only the write
needs serializing.

**Cost accepted.** The author of a published repo can add global skills by
pushing upstream and syncing, without a second admin act — accepted on the
one-team trust boundary (§5.29), because a group whose scope stays coherent
is worth more than a review of each added file. The same repo imported by two
people is two independent groups whose qualified names collide and resolve
own-over-global.

Rules: [Skills](../reference/protocol.md#skills--apiv1skills) in the wire surface.

### 5.32 A project's environment is write-only, like every other credential here

Decided 2026-08-26.

**Decision.** A project's environment — the variables its container is
created with — is sealed at rest, masked in every response, replaceable but
never readable back. Names stay plaintext so one variable can be rewritten
without retyping its neighbours and the audit log stays answerable. The seal's
AAD is the project id, so a ciphertext pasted into another project refuses to
open there rather than acting as a decryption oracle for an attacker with DB
write access but not the key.

**Rejected.** A per-entry "secret" flag masking only marked values — it would
make this the ONE credential surface whose visibility is a per-item choice,
against provider keys, MCP secrets, SSH passwords and trigger secrets, which
are all unconditionally write-only; and a forgotten flag writes a token to a
readable field silently, a failure with no upper bound.

**Cost accepted.** Confirming that `TZ` says what you think takes a look
inside the container — one `env` away in the terminal the workbench offers,
and the honest place to look, because the environment is readable to
everything running in that container anyway. Sealing defends the database
and the screen, never hides a value from the model, and the UI says so.

Rules: workbench invariant 27 (the environment is content, not identity).

### 5.33 Storage is a volume the delete destroys

Decided 2026-08-28. The target/template split this entry introduced was
reversed the same week — §5.36 holds what replaced it. What stands:

**Decision.** One runtime axis: the runtime generation lives only on the
PROJECT, and a content change to a sandbox bumps it on every project naming
the row (`ProjectStore.BumpRuntimeGen`), so the instance cache, the terminal
fences and `RetireProject` watch exactly one thing. Storage is a volume,
always — a container runs as the image's user (root unless the sandbox says
otherwise), the container is the isolation boundary, and its files live in a
volume nothing else mounts. A project delete destroys its storage: a volume
nobody has a listing for is an unbounded leak, and the row was its only
handle. No project, no sandbox tools: an agent without one is a chat. The
session binding is `project_id` alone — a project pins its machine, so a
second column could only disagree.

**Rejected.** The local daemon's bind mount (`--workspace`, the `DOCKER_HOST`
guard, the operator uid:gid default) — that default kept the container
unable to install a package into itself. A per-owner "scratch" project for
unbound runs — it made "which tree did that command touch?" surprising. A
sandbox generation beside the project generation — two maps that must not
reach each other's rows.

**Cost accepted.** The tree is no longer a directory on the operator's
machine; `docker cp` and the export route are how it comes out.

Rules: workbench invariant 27; [Projects](../reference/protocol.md#projects--apiv1projects).

### 5.34 One E2B-compatible backend, written here, not one backend per cloud

Decided 2026-08-28; verified against E2B's cloud and Alibaba Cloud Function
Compute.

**Decision.** Function Compute's cloud sandbox is E2B SDK compatible across
everything the workbench needs, so the second backend is **one backend that
speaks the E2B API** and a sandbox row naming the service — `api_url`,
`domain`, `api_key` — with no `flavor` discriminator: the moment one appears
that configuration cannot express, it is a new decision, not a switch to
grow. The client is written here — six REST calls and Connect-over-JSON,
~150 lines of standard library — which keeps `sandbox/e2b` in the ROOT module
(§5.7). The sandbox is remembered, not searched for: its id lands in
`projects.instance_ref` before the client will use it, and a failure to
record fails the create, since an unrecorded sandbox is billed compute nobody
will ever stop. The lease is extended on demand — every control call sends
`max(configured TTL, the operation's own bound)` — never by a keepalive. Stop
is pause and Reclaim is kill: the sandbox IS the storage, so killing it is
the whole of §5.33's delete, and `auto_pause` defaults to true. Every create
asks for a per-sandbox token (`secure: true`), because without it E2B's
daemon takes no credential at all.

**Rejected.** One backend per cloud — the services differ in three fields. A
community Go SDK, or a protobuf toolchain with generated stubs — two module
dependencies for six messages; generate them if the surface grows past that.
A metadata query to find a sandbox — a filter syntax the compatible services
do not document identically. A keepalive goroutine — the extension rides the
control call the operation already forces.

**Cost accepted.** The hand-written codec is checked against a fake and a
probe, not a schema. A terminal idle past one full lease can lose its
sandbox. Pausing on Function Compute is gated behind a per-function feature,
and the client passes the service's refusal through verbatim.

Rules: [Sandboxes](../reference/protocol.md#sandboxes--apiv1sandboxes); the
services' rendering quirks live on the code that absorbs them (`sandbox/e2b`).

### 5.35 A port preview is a gateway with a grant, not a published port (retired)

Retired 2026-08-31 (decided 2026-08-28). Was a grant-token reverse proxy from a
sandbox port to the browser (`sandbox.PortForwarder`/`PortDialer`, `ports`).
The shipped compose topology publishes a port on the daemon host's loopback,
unreachable from a containerized server — it worked on some deployments and
silently failed on others; a headless browser in the sandbox replaces it.

### 5.36 A sandbox is one row, and only its identity freezes

Decided 2026-08-28, reversing §5.33's target/template split the same week it
landed.

**Decision.** One `sandboxes` row carries where it runs and what runs on it,
and a project names one. The mutability line is drawn between FIELDS instead
of between tables — which is where it always was: only the type and the
destination ever froze, while the credentials sat in the "frozen" table and
were always editable. A project may still change its image by moving between
sandboxes that share a destination, and no further — the files live at that
address and do not travel.

**Rejected.** Separate `sandbox_targets` and `sandbox_templates`, justified
by reuse — for the common case there was nothing to reuse (a local docker
target's whole config is `{}`, so pairing it with a template was ceremony on
every project create), and the split generated a bug class of its own: a
target and a template of different types, needing a type check on the
project write, another on the health check, filtered dropdowns in two places,
and still reaching a screen as `unknown sandbox target type: e2b`.

**Cost accepted.** A second image on one remote daemon repeats that daemon's
host and credential, and rotating a key touches every row that carries it —
against a two-dropdown project create paid on every use. `Duplicate` copies
everything but the identity and the credential, which is dropped rather than
carried as a mask that would resolve to empty and look copied.

Rules: workbench invariant 45 (which fields freeze, and why e2b freezes three
more).

### 5.37 The E2B sandbox defaults to no network

Decided 2026-08-28.

**Decision.** The `sandbox` package promises isolation by default and the
docker backend keeps it (`NetworkMode("none")`), so the E2B create sends
`allow_internet_access` explicitly on every create — `false` unless the
sandbox opts in. Both backends read the same way: an un-opted-in sandbox has
no outbound network.

**Rejected.** Omitting the field and inheriting the service's own default —
which is internet ON.

### 5.38 A workbench docker sandbox caps memory and CPU by default

Decided 2026-08-28.

**Decision.** A docker sandbox whose config leaves `memory_mb` or `cpus` at
`0` gets the workbench's default cap (4096 MiB, 2 CPUs) in
`sandboxes.applyImage`: `0` means "this default", never "unlimited". Agent
code runs in that container, and an uncapped one is a host-DoS surface — a
fork bomb takes the machine, and on a shared workbench that is everyone's.

**Rejected.** Putting the default in the SDK's `sandbox` package — its
isolation-by-default promise covers network, filesystem, capabilities and the
per-command timeout, and a library embedded on its own has no context to
assume it is running untrusted code for many users.

### 5.39 The SDK reads no environment variable of its own

Decided 2026-08-29.

**Decision.** The `agents` package calls no `os.Getenv`; every knob is passed
in, and the trace toggle `Observe.IncludeSensitiveData` reads nil as include.
What a wrapped vendor library (openai-go's `OPENAI_API_KEY`) or an OS tool the
docker backend drives (`SSH_AUTH_SOCK`) reads is its own visible, overridable
contract — the distinction is authorship: the SDK decides nothing from ambient
state.

**Rejected.** A nil toggle falling back to
`OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA` — the one place the package read
process environment, contradicting the stated stance
([Models](../howto/models.md): no global registry, no init hook, no ambient
default) and fighting the embedder, who always passed an explicit value and
carried a comment saying the variable "is not consulted".

**Cost accepted.** Turning tracing content off is now `IncludeSensitiveData:
new(false)`, an explicit per-run decision — which is the point.

Rules: spec §2.14.

### 5.40 A handoff acknowledgement tells the target it owns the turn

Decided 2026-08-30.

**Decision.** The function-call output the runner synthesizes for a handoff
carries the transfer marker `{"assistant": <target name>}` AND a plain-language
line, `You are now "<name>", handling this conversation directly.` Small
models act on the sentence; large ones are unaffected by the redundancy.

**Rejected.** The marker alone — a weak model reads it as the output of a
tool *it* called and narrates the transfer in the third person (observed: a
small flash model saying it had "transferred the question" to the very agent
it had just become). Prose alone — trades a machine-readable signal that
keeps the lineage for nothing.

Rules: spec §2.4.

### 5.41 ChatGPT login redeems a pasted callback URL, not a loopback listener

Decided 2026-08-31.

**Decision.** OpenAI's Codex OAuth client registers only loopback redirect
URIs, so the authorize URL still names `localhost:1455`, but nothing listens:
the redirect fails to load, the user pastes its URL back, and the server
redeems the code against the PKCE verifier it stored by `state`. The standard
headless-OAuth pattern, behaving identically whether the server is local or
remote.

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
key (`attachments/<owner>/<uuid v4>.<ext>` — v4 deliberately, since v7's
timestamp prefix narrows a brute-force window). Entries store a sentinel
reference; a `ModelProvider` decorator hydrates it at the model boundary,
the one seam every path crosses (fresh, resume, replay, compaction,
fallbacks). sigv4 is implemented in-repo (~150 lines, PUT and DELETE),
verified against an openssl reference vector.

**Rejected.** The database as the only backend — every turn re-inlines every
image as base64, and a local-first server cannot hand a provider a
`localhost` URL. Presigned URLs — both providers cache prompts by prefix, so a
URL that differs per request re-bills the whole history after an image, and
an expired signature 404s a replay. Hydrating in the entry store's load — it
covers only HISTORY; the current turn's input and a resumed state's input
reach the model without a storage read. The AWS SDK — the server module's
heaviest dependency for two calls.

**Cost accepted.** Anyone holding a link can read that image; the setting
says so. The scheme constant lives in `store` beside the row it names,
because `attachments` → `settings` → `store` leaves the client package
unable to own it without a cycle.

Rules: workbench invariants 56–58.

### 5.43 Config booleans are stated positively

**Decision.** Every boolean in stored configuration — JSON, REST bodies, the
panel — names the capability it grants, and `true` turns it on. A default-on
knob added late is a `*bool` where nil (the key absent, every row predating
the knob) means the default; the job of "zero value means unchanged" is done
by type, not by name. The SDK keeps Go's own idiom for zero-value structs
(`Agent.DisableToolChoiceReset`, like `http.Transport.DisableKeepAlives`),
and the bridge flips polarity in one place.

**Rejected.** Negated flags (`disable_x`) on the config surface — they
existed only to keep a late knob from flipping existing rows, which the
pointer already does. The old keys decode past silently, the
`compaction_threshold` precedent.

### 5.44 Middleware and sessions: the session is the memory, not the input

**Decision.** With a session attached, a re-entering middleware sends only
what the session does not yet hold. `Loop` sends the evaluator's feedback
alone — the attempt it judged completed and is persisted. `Retry` keys on the
SDK's own announcement: the user-input save emits `ItemsPersistedEvent` like
every other save that leaves nothing behind, and an attempt that announced one
stored the input.

**Rejected.** Rebuilding the next attempt's input by hand (`Loop` feeding the
whole attempt back through `ToInputList`, `Retry` re-sending the original
input) — right without a session and wrong with one: the loop prepends the
session's history to every attempt and persists the new input ahead of the
first model call, so the model saw the prompt twice and the prior turns three
times over. Keying `Retry` on "a session is attached" — a transient failure
ahead of the save (a session read, a tool listing, a start hook) would then
retry without the message.

**Cost accepted.** Announcing the user-input save widens an existing
contract; the one consumer that mirrors persisted state from it (the
workbench's stream bridge) resets buffers that are still empty.

Rules: spec §2.5, §2.12.

### 5.45 A middleware resumes under the caller's control

**Decision.** `RunInput` carries the caller's `RunControl` (`Control`), and
`ResumeRunWith` continues a paused run under it, so `middleware.Approval`'s
resume leaves the handle `Run` returned live. The control's queue is carried
as is, never reseeded from `RunState.PendingInput` — the pause copied the
queue into the state without draining it, so a reseed would deliver every
item twice — and `ResumeRunWith` panics on a control this package did not
mint: the interface exists so a host can hold one, not implement one.

**Rejected.** Resuming through `ResumeRun`, which mints a fresh control —
correct for a serialized state in a new process, wrong in-chain: every
`StopAfterTurn` or `Steer` on the original handle after the first policy
resume reached a run that had already ended, and the caller had no way to
know.

Rules: spec §2.11b, §2.12.

### 5.46 A tool panic takes the tool-error path

**Decision.** The recover lives in `invokeTool` on both the timed and untimed
paths, so a panic is an error from the call like any other and takes the one
error tail (`IsError`, output guardrails, span error, the valve of spec §2.7d).
The per-call goroutine's own recover is only a net for a panic outside the
tool body, and that net aborts the run — it is not the tool's failure.

### 5.47 Zero-setter sandbox options were removed

Retired ledger. `ExecRequest.Stdin`, `docker.Options.ContainerWorkDir` and
the `path` parameter of `Exporter.ExportTar` had no setter anywhere in the
repository and were cut under scope §1.2's zero-consumer rule; each comes back
the day a caller needs it, as an option with that caller.

### 5.48 apply_patch parks a large file instead of snapshotting it

**Decision.** apply_patch's atomicity rests on an in-memory snapshot of every
file it touches, so a failed commit rolls each one back — and a Delete of a
file over the read limit, the one operation that needs no content, is parked
by an atomic rename instead of refused. `Sandbox.Rename` stays on the
interface for it. Update and Move are not parked: they need the content, and
the read limit is the limit.

Rules: spec §2.7s.

### 5.49 The Anthropic adapter decides its output items at the stop reason

**Decision.** Three translation choices share one reason: the runner reads a
turn as ONE assistant message and executes its tool calls before it looks for
a refusal, while the Messages API reports its verdict LAST. So consecutive
`text` blocks become one message item with a part each; when streaming,
`output_item.done` is emitted only at `message_stop`, from the same
`convertOutput` the blocking path uses; and a `refusal` part in replayed
history is dropped rather than sent back as assistant text — a refusal is not
an answer the model gave, and replaying it as one teaches the next turn that
it was.

**Rejected.** A message item per text block — the runner keeps only a turn's
last message, so every text but the last was silently dropped. Per-block done
events — they leaked a `function_call` done for a response whose stop reason
turned out to be `refusal`, an item the terminal output rightly did not
carry, breaking the contract that the two are interchangeable.

**Cost accepted.** Finished items wait for the verdict; text deltas still
stream live. The other lossy input translations are listed in
[Models](../howto/models.md).

Rules: spec §2.15.

### 5.50 Trace payloads are content-addressed per session, not stored per span

Decided 2026-09-02.

**Decision.** A span's payload elements — an input item, a reply item, a tool
definition, the system prompt — are stored once per session in `trace_blobs`
under their sha256, and the span row keeps metadata plus a packed list of
hashes. Blobs are keyed `(session_id, hash)` and never shared: per session,
every lifecycle operation — delete, fork, retention — is a whole-session one,
and no reference count or sweep exists. Elements are split by shape, not by
type — an array is one element per item, anything else one element — so the
store knows nothing of the SDK's item types.

**Rejected.** A payload per span — every model call re-stored the whole
conversation, so a session's trace grew with the square of its length (a
200-turn session with 1 KB items ran to some 60 MB of `input`), and the row
cap bounded one span, never the sum. Global content addressing — it would
dedupe tool schemas across sessions, but needs a reference count or
mark-and-sweep with a concurrency story, and muddies per-user erasure. A
reference table — a row per reference costs ~150 bytes with its index, five
times the hash it points at.

**Cost accepted.** A copy of the tool schemas per session — kilobytes,
against the quadratic term removed. The replay body cap no longer derives
from the span cap and is a constant. This is the shape of LangGraph's
checkpointer (`checkpoint_blobs` per thread), with a content hash where it
uses a version.

Rules: workbench invariant 62.

### 5.51 Off-chain history is decided by position, not provenance

Decided 2026-08. `openai.CompactionSession` rewrites history from the server's
response chain, and a rewrite deletes whatever the chain never saw, so the
runner reports every way the log can outgrow the chain through one flag,
`CompactionArgs.OffChainItems` — four cases, each measured, none assumed.

**Decision.** Position (anything after the last model-produced item) is
decided by position alone: a steer taken after the final output is external
input that reached no model call, so provenance misclassifies it. A truncated
read is reported only when the prepare-time read came back FULL, the one
observable that says the window cut something; a log exactly the window's
size reads full too, so the rule errs toward reporting. A handoff filter is
reported whenever it RAN, without inspecting its output: an identity filter
and one that redacts in place leave the length untouched, and a comparison
that got it wrong deletes the original unread. A withholding projector is
measured per entry, not per config: a projector that REWRITES an item is not
withholding it, and "a projector is installed" would never clear. The last
three ride on `RunState.OffChainHistory` because a resumed run re-reads no
history and re-runs no filter, and answering from the resumed options is
silently false whenever the caller did not repeat `Conversation.Settings`;
position clears between runs and is recomputed. The guarded swap compares the
highest sequence the store HOLDS, not the highest ever issued, or a session
emptied outside the SDK would refuse every replace forever.

**Rejected.** A runner-side skip of the pass when the flag is set: it takes
the decision away from a storage with no chain to be wrong about, and an
agent that always finishes through a terminating tool would never compact.
Detecting identity filters by comparing output. Keying the window case on "a
limit is configured": a log that never reached its window would be mistaken
for the pin-plus-window conflict. Retrying or merging a pass that lost the
sequence comparison: compaction is housekeeping, one skipped pass costs size.

**Cost accepted.** A caller who PINNED `CompactionModePreviousResponseID` and
configured a read window gets the pass abandoned every run
(`abandoned: off_chain_items`) while the log grows — only the caller can
resolve it, by dropping one of the two. A store without `GuardedReplacer`
keeps the unguarded swap: refusing to compact for it would take the feature
from every third-party store rather than from the race.

Rules: spec §2.5f

### 5.52 Overflow recovery writes on the side the pass can survive

Decided 2026-08. Overflow recovery rebuilds the turn's context from the log
and throws the in-flight items away, so the turn must be written to the
session first — or the retry hands the model a conversation the caller's
steer never reached while the next write past its mark counts it delivered
(§2.11b). WHEN it is written depends on which recovery applies, and the two
want opposite answers.

**Decision.** A `Compactor` reads the log and returns a projection of it, so
the turn has to be IN the log before the pass: write first. A
`CompactionAware` storage may answer with a replacement built from its own
response chain, on which nothing produced locally stands, so a write made
first is a write the pass deletes — stored, counted delivered by that very
write, then gone, with nothing in flight to roll back: write after the pass,
then read the log once more so the turn stands on the compacted history. The
path is chosen up front from whether the storage compacts itself. A forced
self-compaction buys a retry only if the context came back weighing strictly
less (summed stored bytes over the same windowed read the model gets): a
saturated window hides growth perfectly — a storage that abandons its
replacement mid-pass leaves one extra entry, which pushes the oldest out of
the window and comes back the same LENGTH, while that append is exactly what
makes the history "changed" — so neither the count nor "did anything change"
decides it, and "strictly less" rules the no-op out on its own. Bytes are a
deliberately conservative proxy for tokens.

**Rejected.** Treating every 400 as an overflow: it would compact and retry
after a malformed request, hiding a bug behind a shrinking conversation.
Writing the turn when no recovery is available: there is no pass to prepare
for, and the write would only spend the rollback the failing run is about to
want. Retrying on a no-op pass: an identical request fails identically.

**Cost accepted.** `MaxRetries` is zero by default, so an overflow is reported
unless the caller opts in. A write that fails abandons the recovery with a
`compaction_failed` diagnostic rather than retrying blind. A pass whose result
does not weigh less costs a retry the run would have spent on a request that
already failed. Anthropic's success-shaped `model_context_window_exceeded` is
surfaced as an error carrying the marker: resending unchanged stops at the
same wall.

Rules: spec §2.5g, §2.15

### 5.53 Plan mode denies rather than hides

Decided 2026-08 with the `Plan` middleware. While a run is planning, a tool
outside the read-only set stays in the model's toolset and answers a call with
a refusal naming `submit_plan`, as a normal tool OUTPUT.

**Decision.** A model carries priors about tool NAMES and reaches for them
unprompted: a hidden tool gets called anyway, and "tool not found" teaches it
nothing about the phase — it cannot tell a gated tool from one this session
never had. The refusal is an output because an error without
`FailureErrorFunction` aborts the run, and a phase decision is not a failure.
Handoffs are the deliberate asymmetry, hidden via `Handoff.IsEnabled`: a
target's full toolset is a side door out of plan mode, and a model has no
priors about THIS agent's handoff targets, so hiding one wastes no turn. An
MCP tool's `readOnlyHint` is a claim an outside server makes about itself,
and "nothing changes until you approve" cannot rest on it; admission is by
the caller's `ReadOnlyTools` name, a statement of trust that is the caller's
to make. The refusal outranks approval because the approval partition runs
before a tool invokes: a gate on `OnInvoke` alone would pause a human over a
call the phase then refuses, so `Apply` translates `ApproveTools` into
per-tool predicates the phase can suppress. `PlanPhase` is per run because
the SDK has no notion of a session; `OnUnlock` exists so a host can keep its
own record and `Unlock` before the run. `Plan.Apply` is unconditional so a
host decides plan mode outside the agent and still rebuilds the same agent
for a durable resume — a rebuild happens AFTER the unlock and must carry the
`submit_plan` the paused state names. The host persists the UNLOCK, as the
unlock's precondition: the approval ledger records approvals whose execution
then failed, and tool output text can be rewritten by a guardrail.

Only a PERSON turns plan mode on: the gate's value is "a human looks before
anything changes", and a model that judges "simple, no plan needed" is the
failure the gate exists to catch.

**Rejected.** Hiding gated tools. A second pause mechanism for plan review:
`submit_plan` is an ordinary approval-gated tool. A session-scoped phase in
the SDK. Trusting `readOnlyHint`. Per-item `todo_write` updates: sending the
whole list is simpler to prompt for and impossible to desynchronize, and a
malformed list refused whole keeps `OnUpdate` from seeing a half-applied state.

**Cost accepted.** A gated write tool spends a model turn on a refusal. A
read-only tool named in `ApproveTools` keeps its approval in both phases.
Nothing checks that a tool claiming read-only behaves. `Loop` needs
`RunResult.StoppedEarly` to tell "finished" from "stopped", so the stop is
never cleared and is reported wherever the run ends.

Rules: spec §2.12

### 5.54 A task's ending is claimed, not observed

Decided 2026-08 across the task_retry, task_stop and workflow-as-task work. A
task's terminal state is won by compare-and-set, and every consumer acts on
the transition it claimed, never on a row it read.

**Decision.** Finalization is a CAS because a read-modify-write cannot
arbitrate two finalizers — hence no file-backed store. Reopening a terminal
task for retry removed the invariant "non-terminal means the current run", so
every attempt-scoped write names its run id: a stop that read the row just
before a retry would otherwise cancel the new attempt while its run kept
executing, unkillable, and an approval persisted before its pause landed
would pause, reclaim or reap the attempt that replaced it. A stop reports
what it DID because a host asked to stop a run it has never heard of
(ordinary during a launch) can only answer success, which read as "it will
wind itself up" leaves the task running to completion unrecorded;
`StopAlreadyFinished` neither writes a
cancellation (overwriting a real completion, with the retry it earned) nor
ends the call ("that run is over" is also what a stop hears after a retry
landed). A late outcome and a lost one are the same dead run under a live
row, so the stop waits briefly and boundedly, then cancels: waiting keeps a
real completion, the bound keeps a task whose outcome never landed from being
un-stoppable. Compensation consults whether `OnRunFinished` spoke, since a
quick finish and a run ended while the host was unreachable leave the same
terminal row. Delivery counts on the model's path only: a
result that landed after the answer was decided is unseen, and a person
reading it over HTTP has told the model nothing. A failed launch is released
rather than counted, or a shutdown would spend the ceiling on runs that never
existed. The sweep runs before requests are accepted because `FailOrphans`
would declare a just-claimed retry's fresh run dead. Notification fields
escape both delimiters, or a crafted result could re-aim the task id and
status on its own line.

**Rejected.** A file-backed task store. A second lifecycle beside tasks for
step sequences and loops, or a fifth model-facing verb: two tools that both
mean "start background work" are the tool-choice errors a small model makes.
A precomputed `retryable` boolean (it lags a round trip; capacity changes
between offer and click). Cancelling on the row alone.

**Cost accepted.** One stop chases at most one retry. Two processes sharing
one store keep the sweep-vs-retry race. A durable host's debt-row guarantees
cannot live on the interface (an in-memory store has no debt), so they are
spec text. A stop against a genuinely lost outcome waits out the bound.

Rules: spec §2.13

### 5.55 Fan-out buffers per subscriber, not per channel

Decided with `Fanout[T]`. One producer's events reach many consumers through
per-subscriber buffers; a slow subscriber loses items and is told so.

**Decision.** Fan-out is a requirement, not an optimization, and that was
measured rather than assumed: a slow consumer couples to the producer under
`iter.Seq2` (13.1× the ideal wall clock), and it also couples under a buffered
channel, just later — with `chan(64)` the producer still finished at 992 ms
against a 100 ms ideal once the buffer filled. Neither stream shape isolates a
slow consumer on its own, so per-subscriber buffering is needed either way. A
drop is always announced as a `*GapError` because a consumer cannot otherwise
distinguish a timeline missing content from one that never had it. A cursor
ahead of the head is a timeline reset delivered immediately on subscribe,
because the stream a stale cursor lands on has often already ended and a gap
waiting for a delivery that never comes leaves the consumer in exactly the
silence it exists to break; it must not read as `AtEnd`, which would tell a
consumer to stop reading a run that is still going, and its sequence must
never run backwards past the deliveries that follow it. `Close` waits for a
publish already accepted because that item has a sequence number and sits in
replay with no gap to report it.

**Rejected.** Dropping silently — corrupts the consumer's view undetectably.
Disconnecting the slow subscriber — turns a recoverable hiccup into a visible
failure. A single shared buffer.

**Cost accepted.** Memory per subscriber. The zero-value item beside an
`AtEnd` gap, which a forwarding consumer must skip (a nil pointer, for a
stream of pointers).

Rules: spec §2.11

---

### 5.56 Compaction's unit of work is a group, not an entry

Decided 2026-08 with `agents/compaction`.

**Decision.** A function call and its output belong together (the API rejects
one without the other), and so do a reasoning block and the tool call it
precedes. Entries are grouped first; a strategy only ever includes or
excludes whole groups, so cutting through a pair is not a mistake a strategy
can make. An exclusion is `settled` once a later model call has priced it in,
so the size estimate stops subtracting what the newest usage never counted.

**Rejected.** Per-entry strategies with a pairing check afterwards: every
strategy re-implements the check, and one forgets.

**Cost accepted.** A group is the smallest thing a pass can drop, so a large
tool output is kept or dropped with its call.

Rules: spec §2.5f.

### 5.57 Delivery of a background result is a debt, not a call

Decided 2026-08 with the workbench's task plane (invariant 32).

**Decision.** A task finishes while its parent session may be busy, paused on
a human decision, or gone with the process, so "session S is owed a turn
carrying P" is a durable row written in the same transaction as the task's
terminal status; the SDK keeps no debt of its own and only reports endings
and deliveries, because when a session may be interrupted is host policy.
Debts drain when a session can take a turn (end of any run on it, startup),
and one drain pays every debt with the configuration snapshotted from the
agent that ASKED, so three results landing while a person types produce one
turn, through the agent that started them. A cancelled task owes nothing.

**Rejected.** A callback at completion time: it lands mid-run or on a paused
session, or never, after a crash. Draining per debt: three turns for three
results.

**Cost accepted.** A result waits for the next turn boundary; `FailOrphans`
must run before requests are accepted, and the drain after handlers are wired.

Rules: workbench invariant 32.

### 5.58 A model-authored workflow lands only through an approved save, by name

Decided 2026-08 with `save_workflow` (invariant 39).

**Decision.** Authoring is a WRITE to configuration, so the tool carries
`NeedsApproval` itself, not the agent's `approve_tools`; its approval
predicate runs the same resolve the write does, so an unsaveable proposal
executes at once into a refusal the model reads, and only a store fault
still asks a person. The pair is per-agent opt-in and chat-only: a
background run has nobody to approve. The model addresses workflows by NAME,
the server owns ids and reuses a kept step's id on update so a retry in
flight keeps naming the same step; same name means the same workflow, so a
save is an upsert. The approval card shows the proposal as it will be
stored, not as the model spelled it.

**Rejected.** A schema on every agent's every request. Model-chosen ids.
Letting the model switch the gate off.

**Cost accepted.** A proposal that needs a person costs a pause even when the
change is trivial.

Rules: workbench invariant 39; authorization per §5.29.

### 5.59 A schema change recreates the database

Decided 2026-08 (invariant 25).

**Decision.** No migrations: a structural change means dropping and recreating
the database, a dev-tool stance taken deliberately. The stance is honest only
if a mismatch is loud, which is what the startup zero-row probe buys.

**Rejected.** `ALTER TABLE` migrations — a second schema language to keep
correct for a store that is rebuilt anyway.

**Cost accepted.** Production use needs this decision reversed first.

Rules: workbench invariant 25.
