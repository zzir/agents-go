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

Retired as a ledger 2026-09-04: the renames it listed rode the v0.3.0 window
and live in the release notes. What survives: a name earns a rename only when
it misdescribes or breaks a Go rule, never to "look less like Python"; a
rename is a breaking change batched into a window users absorb once, and the
next window is the next breaking minor (§5.8); an openai-go major, if one
comes, rides in it (§5.5b).

### 5.3 `Instructions` is a func type

**Decision.** `Instructions` is a **func type**: `StaticInstructions` covers
the fixed case, `WrapInstructions` composes, and resolution (nil handling) is
the runner's job behind unexported entry points, not API surface. The
stored-`Prompt` half was retired 2026-10-03 (v0.5.0) with OpenAI's shutdown of
reusable prompts (2026-11-30); prompt text lives in `Instructions`.

**Rejected.** Single-method interfaces with `...Func` adapters — their only
implementations were unexported types in this package, a plug point nothing
ever plugged into; a func type is the same capability assigned directly. The
same rule collapsed `tasks.AgentResolver`, `Launcher`, `Stopper` and
`WakeGuard`; `tasks.Store` (multi-method) keeps being an interface. A
single-method injection point is a func type unless a second method is
already in sight.

**Cost accepted.** A program that bound a stored prompt pastes its text into
`Instructions`; variables become string formatting on the caller's side.

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

**Cost accepted.** openai-go retypes fields inside v3 minors (v3.54 did, to a
function_call_output's `CallID`). `internal/oaicompat` absorbs it so the floor
stays put; a CI job builds against `@latest` to catch the next.

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
inversion, so the split moved no import path.

**Cost accepted.** A submodule is a separately released module: every release
tags `<dir>/vX.Y.Z` beside `vX.Y.Z` on the one commit whose `go.mod`s require
the root at `vX.Y.Z` (the `replace` stays, for CI), and a consumer-smoke job
proves each `go get` once the release is out. A submodule pseudo-version from
between releases may not build against the root it names.

Rules: [Architecture](architecture.md#module-boundaries).

### 5.8 Public API compatibility begins at v1.0.0

**Decision.** Any release before v1.0.0 may break exported identifiers; a
release that breaks bumps the minor (v0.x.0), so a patch release (v0.x.y)
carries only fixes and additions. Each break is recorded in the release notes
with the old spelling beside the new, and breaks are batched into as few
releases as the work allows, so a user absorbs one migration rather than a
drip.

**Rejected.** A deprecation cycle before v1.0.0 — it was promised once and
not kept through the structural collapses, and a rule nobody follows teaches
the reader that this document describes intentions rather than behavior. The
cycle begins when the API stops finding its shape. Letting a patch break —
`go get -u=patch` is the one upgrade Go users treat as safe.

**Cost accepted.** A fix that needs a break waits for the next minor or ships
as one. `scripts/release-check.sh` compares the exported API with the previous
tag before a patch is tagged, and `release.yml` runs it again; a refusal there
comes after the module proxy has the version, so the patch is retracted and
the next minor tagged on the same commit. A constant's changed value passes.

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
**panics**; one whose input is runtime data **returns an error**. `NewTool`,
`AgentAsTool` and `OutputType` derive their schema from a Go type —
deterministic per type, so a failure is a bug any test surfaces immediately
(the `regexp.MustCompile` precedent), and panicking keeps them chainable
inside `Agent{...}` literals. `NewRawTool` and `NewDynamicOutputSchema` take a
schema that is data, so they return an error.

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
for persistent containers — it cannot see a tmpfs mount (the `/tmp` the
backend mounts), so a file exec had just written there read back as absent; a
volume is visible, which `ExportTar` relies on. Every file operation goes
through `exec` instead.

**Cost accepted.** Docker bind-mount mode is the one exception: its file
operations run on the host side of the mount, so there they are confined to
`WorkDir` via `os.Root` and translated from the in-container mount point —
`sandbox.ErrOutsideWorkDir` rather than a silent re-rooting.

The docker backend's three-way file dispatch is written out in each method: an interface over two backends never chosen dynamically would hide which one runs without removing a branch.

Rules: spec §2.7t.

### 5.15 Streaming-only backends adapt with a Model decorator

**Decision.** A backend, or its client SDK, that refuses non-streaming
requests — the ChatGPT Codex backend returns 400; anthropic-sdk-go refuses
`max_tokens` above 21,333 client-side — is adapted by `NewStreamOnlyModel` /
`NewStreamOnlyProvider`: `Respond` runs the request as an internal
`StreamResponse` and assembles the final response from the terminal event,
sharing the runner's own `responseAssembler` so the two paths cannot drift. It
composes **innermost**, directly on the backend, so retry, fallback and
routing above it see a severed stream as an ordinary `Respond` error. The
Anthropic adapter's `Respond` is always served this way.

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

### 5.21 A dead shared connection repairs itself, and the redial never repeats a tool call

**Decision.** Nothing in the go-sdk re-establishes a dead session (it only
resumes an interrupted SSE stream), so the connection owns its own recovery:
given `mcp.Options.Redial`, a session found dead is replaced **in place**, so
every holder of that server recovers rather than only the runs that start
afterwards. Death is noticed by watching the connection, not by a caller
tripping over it; healing is throttled; and the redial repeats only idempotent
work — `tools/list` is re-issued, a failed tool CALL is reported to the model,
because a dead line cannot say whether the server ran the tool, and running a
write twice is worse than reporting it once.

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
clients with `WithMaxRetries(0)`; the one retry layer for a model call is
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

Retired 2026-08 as a ledger of cuts under the standing rule that a
zero-consumer feature is removed, not kept (`tracing/otel`, `filesession`,
`tools/bravesearch`, `cmd/verify`, MCP serve).
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

Rules: workbench invariant 45 (only a sandbox's identity freezes); invariant
27 (a session binds a project).

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
shapes to keep equivalent. Sessions, forks and tasks on one project share its
tree; a regenerate or fork rewinds the transcript, not the files.

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
admin approved it. Member-supplied URLs (MCP endpoints, provider base URLs,
skill imports) get **no private-network/SSRF defense**: one team, one trust
boundary, and egress control is applied outside the server.

Rules: workbench invariant 42 (the in-write re-check); the status matrix and
list ordering are in [the wire surface](../reference/protocol.md#authorization).

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

**Decision.** An import lands a whole repository's `SKILL.md` files at once,
and two repositories may each ship a `review`, so **the repo is part of the
name**: the model-facing name is `<repo label>:<frontmatter name>`, and
uniqueness keys on `(repo_label, name)` within a visibility context. The
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
fences and `RetireProject` watch exactly one thing. For Docker, storage is a
volume, always — a workbench container runs as root unless the sandbox names
a user (overriding spec §2.7o's image-user default), with all capabilities
dropped; the container is the isolation boundary, and its files live in a
volume nothing else mounts. A project delete destroys its storage: a volume
nobody has a listing for is an unbounded leak, and the row was its only
handle. The session binding is `project_id` alone — a project pins its
machine, so a second column could only disagree.

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

Decided 2026-08-28; verified against E2B's cloud, Alibaba Cloud Function Compute and Bailian.

**Decision.** Function Compute's cloud sandbox is E2B SDK compatible across
everything the workbench needs, so the second backend is **one backend that
speaks the E2B API** and a sandbox row naming the service — `api_url`,
`domain`, `api_key`, `headers` — with no `flavor` discriminator: the moment
one appears that configuration cannot express, it is a new decision, not a
switch to grow. The client is written here — five REST calls and
Connect-over-JSON, a ~150-line Connect codec on the standard library — which
keeps `sandbox/e2b` in the ROOT module (§5.7). The sandbox is remembered, not
searched for: its id lands in `projects.instance_ref` before the client will
use it, and a failure to record fails the create, since an unrecorded sandbox
is billed compute nobody will ever stop. The lease is extended on demand — every control call sends
`max(configured TTL, the operation's own bound)` through `connect`, which
resumes a paused sandbox and only extends a running one — never by a keepalive. Stop
is pause and Reclaim is kill: the sandbox IS the storage, so killing it is
the whole of §5.33's delete, and `auto_pause` defaults to true. Every create
asks for a per-sandbox token (`secure: true`), because without it E2B's
daemon takes no credential at all.

**Rejected.** One backend per cloud — the services differ in four fields. An
auth-scheme switch (`X-API-Key` vs `Authorization: Bearer`) instead of
`headers` — Bailian wants both at once, and `headers` is the E2B SDK's own
parameter. `/timeout` for the extension — Bailian lacks it, and its 404 would
read as a dead sandbox. A community Go SDK, or a protobuf toolchain with generated stubs — two module
dependencies for six messages; generate them if the surface grows past that.
A metadata query to find a sandbox — a filter syntax the compatible services
do not document identically. A keepalive goroutine — the extension rides the
control call the operation already forces.

**Cost accepted.** The hand-written codec is checked against a fake and a
probe, not a schema. A terminal idle past one full lease can lose its
sandbox. Pausing on Function Compute is gated behind a per-function feature,
and the client passes the service's refusal through verbatim.

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
runaway build or a leak can OOM or starve the host (the docker backend already
caps the process count), and on a shared workbench that is everyone's.

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

**Decision.** With OpenAI's Codex OAuth client the redirect a client can name
is loopback-only, so the authorize URL still names `localhost:1455`, but
nothing listens: the redirect fails to load, the user pastes its URL back, and
the server redeems the code against the PKCE verifier it stored by `state`. A
common headless-OAuth pattern, behaving identically whether the server is
local or remote.

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
says so. An install without a bucket has no image input — attachment storage
is a power-user step (scope §1.1). The scheme constant lives in `store`
beside the row it names, because `attachments` → `settings` → `store` leaves
the client package unable to own it without a cycle.

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
alone — the attempt it judged completed and is persisted. A middleware that
must know whether an attempt stored its input reads the SDK's own
announcement: the user-input save emits `ItemsPersistedEvent` like every
other save that leaves nothing behind.

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

**Cost accepted.** `Loop` needs `RunResult.StoppedEarly` to tell "finished"
from "stopped", so the stop is never cleared and is reported wherever the run
ends.

Rules: spec §2.11b, §2.12.

### 5.46 A tool panic takes the tool-error path

**Decision.** The recover lives in `invokeTool` on both the timed and untimed
paths, so a panic is an error from the call like any other and takes the one
error tail (`IsError`, output guardrails, span error, the valve of spec §2.7d).
The per-call goroutine's own recover is only a net for a panic outside the
tool body, and that net aborts the run — it is not the tool's failure.

Rules: spec §2.2 (concurrency), §2.7 (Errors).

### 5.47 Zero-setter sandbox options were removed

Retired 2026-09-02. Was `ExecRequest.Stdin`, `docker.Options.ContainerWorkDir`
and `ExportTar`'s path parameter, none with a setter. Because a zero-consumer
option is removed, not kept; each returns with its caller. See §5.23.

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
stream live. Under a refusal, the items announced past index 0 get no
`output_item.done`, and index 0's finished type may differ from the announced
one (a thinking-first refusal finishes as a message). The other lossy input
translations are listed in [Models](../howto/models.md).

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

Retired 2026-10-03. Was `CompactionArgs.OffChainItems` and
`RunState.OffChainHistory`: what a rewrite built from a server response chain
would delete, reported for `openai.CompactionSession`. Because that session
went — the one path that rewrote history — and nothing else read the flag;
an older state's flag is ignored and the schema floor stays. See §5.52.

### 5.52 Overflow recovery writes on the side the pass can survive

Decided 2026-08. Overflow recovery rebuilds the turn's context from the log
and throws the in-flight items away, so the turn must be written to the
session first — or the retry hands the model a conversation the caller's
steer never reached while the next write past its mark counts it delivered
(§2.11b). WHEN it is written depends on which recovery applies, and the two
want opposite answers.

**Decision.** A `Compactor` reads the log and returns a projection of it, so
the turn has to be IN the log before the pass: write first. A
`CompactionAware` storage may answer with a replacement that keeps nothing of
the newest turn (a reset keeps the newest user message alone), so a write made
first is a write the pass folds — stored, counted delivered by that very
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
nothing about the phase. The refusal is an output because an error without
`FailureErrorFunction` aborts the run, and a phase decision is not a failure.
Handoffs are the deliberate asymmetry, hidden via `Handoff.IsEnabled`: a
target's full toolset is a side door out of plan mode, and a model has no
priors about THIS agent's handoff targets, so hiding one wastes no turn; the
cost is on the request prefix — the unlock changes the system text and the
tool list at once, which a backend that binds reasoning to its prefix answers
by dropping the thinking produced while planning (spec §2.15). An MCP tool's
`readOnlyHint` is a claim an outside server makes about itself; admission is
by the caller's `ReadOnlyTools` name. The refusal outranks approval because
the approval partition runs before a tool invokes, so `Apply` translates
`ApproveTools` into per-tool predicates the phase can suppress. `PlanPhase`
is per run because the SDK has no notion of a session; `OnUnlock` lets a
host keep its own record and `Unlock` before the run; `Plan.Apply` is
unconditional so the same agent is rebuilt for a durable resume, which must
carry the `submit_plan` the paused state names. The host persists the UNLOCK
as its precondition: the approval ledger records approvals whose execution
then failed. Only a PERSON turns plan mode on: a model that judges "simple,
no plan needed" is the failure the gate exists to catch.

**Rejected.** Hiding gated tools. A second pause mechanism for plan review:
`submit_plan` is an ordinary approval-gated tool. A session-scoped phase in
the SDK. Trusting `readOnlyHint`.

**Cost accepted.** A gated write tool spends a model turn on a refusal. A
read-only tool named in `ApproveTools` keeps its approval in both phases.
Nothing checks that a tool claiming read-only behaves.

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
turn, through the agent that started them. A task a person stopped owes
nothing; one a shutdown ended is left working for the restart sweep, which
fails it with its debt.

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

**Cost accepted.** Production use needs this decision reversed first: before
team mode is promoted for production use, or before multi-instance work
starts, whichever comes first. Every release that changes the layout says so
in its release body, and the database must be recreated across such a release.

Rules: workbench invariant 25.

### 5.60 The budget rides on the input, not the instructions

Decided 2026-09 with `ContextBudget`.

**Decision.** The figure the model gets about its own window is appended as
the last input item of every call, a system text item, and is never written
to the session. Every call carries the current number.

**Rejected.** Putting it in the instructions: the number changes every call,
and an instructions prefix that changes defeats prompt caching for the whole
conversation. Threshold reminders at 25/50/75%, a shape Codex added and
dropped within ten days (June 2026) for prompt churn: one input can jump
across a mark unnoticed, and a current figure is what the model actually
reasons with. Persisting it as an entry: it describes the moment it was sent,
replays wrong later, and inflates the history it measures.

**Cost accepted.** Roughly two dozen tokens per call. Before the run's first
call the figure is the host's, the conversation's last measured call, so a
run right after a manual compaction reports the pre-fold number once.
Dropping the previous notice makes a backend that binds replayed reasoning to
its prefix (Anthropic) discard it, so the workbench sends such an agent none:
under reset or hybrid compaction it gets no warning before its window fills.

Rules: spec §2.5i

### 5.61 Retrieval over summary

Decided 2026-09 with `agents/history`.

**Decision.** The model gets two read-only tools over its own session's log,
search and read, so a compaction pass may fold freely: what it folded is one
call away. The log was already kept whole for fork and replay (§2.5f, nothing
is deleted); the tools are a read on that property, not a second store.
Search is a case-insensitive literal substring, newest first, bounded.

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

**Decision.** One memories table keyed by (scope_kind, scope_id, gen, key):
global rows every agent reads, an agent's rows that agent reads, and a
session's rows the model keeps for itself across compaction and a reset.
The rules per kind, injection, who writes, whether the model writes and
after what, size and count, are one Go table (`store.MemoryPolicies`) that
the handler, the run adapter and the injection consult. The model writes
session memory freely and proposes agent memory through the approval gate
save_workflow established (§5.58), under the agent's edit rule (§5.29).

**Rejected.** A separate session_notes table: two concepts for one kind of
thing, a promotion from session to agent scope crossing tables, and a second
tool family later for the memory tool the roadmap already wanted. Notes as
custom session entries: the compaction pass would fold them and the timeline
read would carry them. Notes as sandbox files: a sandbox retires on a content
change, and not every session has one. Model writes to global memory: they
reach every user's every agent; the policy table has the row for it when wanted.

**Cost accepted.** The memories API is breaking (`scope_kind` and `scope_id`
replace `agent_config_id`) and the table is rebuilt, so existing rows are
exported and written back. One table carries three lifecycles, a session's
rows following its fork and delete, an agent's its delete, global's the
database's; the policy table is what keeps them apart.

Rules: spec §2.5i; workbench invariant 64.

### 5.63 A reset is a checkpoint with nothing to say

Decided 2026-09 with `new_context` and the reset compaction mode.

**Decision.** A context reset is the compaction checkpoint the log already
has, with everything but the newest user message in `ExcludedIDs` and the
model's own session memory in the summary slot. The model asks through
`new_context`; the run grants it at the turn's save point, on the persisted
log. Summary stays the default mode; reset and hybrid are an agent's opt-in.

**Rejected.** Resetting mid-turn, when the tool runs: the turn's items are
not yet persisted, so a call could lose its output and the pairing rule with
it; the save point is where the log is whole. A second projection path for
the carried memory: the summary slot already renders up front as a system
message, and the transcript shows what the model kept. Reset as the default:
a provider not trained to keep notes loses the task on the first fold. A
save-point compaction pass for self-compacting storages, so a run could
reset itself when the threshold trips mid-run: it changes the point contract
of §2.5f for every such storage; the budget notice and `new_context` cover
the case this round — `new_context` alone where a host withholds the notice,
as the workbench does for an Anthropic backend (workbench invariant 83).

**Cost accepted.** Two booleans on `RunState` (a schema minor), so a request
made in a turn that pauses for approval is performed when the run resumes,
and the guard below holds across the pause, `new_context` itself
approval-gated included. A reset folds the turn's own tool calls with the
rest, `new_context` included, which is what Codex does too, and which is why
a fresh context refuses another reset until the model has done some work:
the kept user message ("reset now") would otherwise be obeyed in every new
window, seventy times over in the first live run. The checkpoint's first
line says who reset for the same reason.

Rules: spec §2.5i; workbench invariant 65.

### 5.64 Login admission is by verified email, whatever the provider

Decided 2026-09-08.

**Decision.** `--allowed-domains`, `--allowed-emails` and `--bootstrap-admin`
admit an address the provider verified, and that is the whole admission check
for every login provider. A GitHub sign-in is admitted by the account's
primary verified address, the key logins merge on, so one allowlist covers
Google and GitHub alike.

**Rejected.** A GitHub organization allowlist — a second admission key beside
the address, a `read:org` scope on every login, one more API call, and an
organization that restricts OAuth App access answers the membership query
with "not a member" until an owner approves the app. A GitHub handle
allowlist — a handle is renamed at will and is nothing the merge rule keys on.

**Cost accepted.** A team on personal GitHub accounts lists its addresses one
by one with `--allowed-emails`; a domain allowlist admits only the people who
keep an address on that domain as their primary GitHub email.

Rules: [OAuth mode](../howto/workbench-auth.md#oauth-mode).

### 5.65 An agent's override of the system prompt is total

Decided 2026-09-08.

**Decision.** `behavior.override_system_prompt` drops the `system_prompt`
setting from that agent's instructions wholesale: the model's system prompt
is the agent's own text, and an agent with no text sends none. The other
layers — memories, the sandbox prompt, the skills index — keep their own
switches, and the harness's mode layers stay: the plan preamble while the
session plans, the suffix a background run is told nobody reads through. A
handoff target decides for itself.

**Rejected.** Falling back to the global prompt when the agent's text is
empty — the empty case is the point: an agent meant to run on nothing but its
tools has no other way to say so, and "empty means inherit" leaves it
inexpressible. A per-agent copy of the global text — a second home that drifts.

**Cost accepted.** An overriding agent with nothing written runs with no
system prompt at all; the switch's caption says so.

Rules: [invariant 67](workbench-invariants.md).

### 5.66 A retired instance whose container a successor adopted detaches

Decided 2026-09-11 (workbench invariant 27).

**Decision.** A content change bumps the project's runtime generation, but the
docker adoption fingerprint (§5.19) covers only what a container IS — image,
runtime, user, network, limits, environment. A change outside it (a read cap,
an SSH setting) has the successor generation adopt the SAME running container.
So when a retired instance's last holder releases while a successor of the
project occupies the cache, it releases only its connection
(`sandbox.Detacher`), stopping and removing nothing; a deferred user Stop that
new work overtook is superseded the same way. Without a successor it closes as
before.

**Rejected.** Widening the fingerprint to the whole content — every unrelated
edit would replace the container and discard what was installed into it.
Stopping anyway — the successor's commands and shells die mid-flight and its
next call cold-starts the container it was already using.

**Cost accepted.** A successor that replaced rather than adopted sees the old
handle go stale harmlessly. Once a successor exists, only its own idle timer or
stop ends the container.

Rules: workbench invariant 27; [spec §2.7p](../reference/spec.md#27p-stop-keeps-the-filesystem-and-promises-nothing-else).

### 5.67 A list is an array on the wire and JSON text in the column

Decided 2026-09-11 (workbench invariant 1).

**Decision.** An agent's `tools`, `skills`, `handoffs` and
`approval.approve_tools` are `[]string` on the REST API (`store.StringList`),
typed in the OpenAPI document and the generated client, and refused at bind
when they are not arrays. The column stays `text`: the type's Valuer/Scanner
writes the JSON array and reads it back, `nil` as `""`, so a row written
before the change reads unchanged and no schema moves. `skills` keeps its
third value — `null` is "not customized" (every skill the scope can see),
`[]` is none — and so travels without `omitempty`.

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
new message, or when the paused run is cancelled: the `pending_approvals`
row is deleted (the claim a racing decision loses), the calls it waited on
persist as `tool_call` annotations whose display carries `not_run` with the
reason, and the hub ends the record with `run.cancelled {reason}` —
`superseded` or `stopped`. The UI resolves the cards from the live event,
from the stored marker on reload, and from the newer run's `run.started`
when the event never came (a restart between the pause and the message).
A background task's paused run is its task's to stop and is left alone.
A trigger's agent turn refuses while a pause stands, as a wake-up does.

**Rejected.** Refusing the send (`409`) — the composer sat locked on a
question the person had moved past. Letting both stand — the later approval
resumed the old `RunState` and its answer landed after the newer turn, out
of order and out of context. Persisting the pending calls as items — an
abandoned call must not enter the model's history.

**Cost accepted.** A newer message discards a pause by design; the cards say
so. A stale hub record on a restarted server publishes nothing, so the
client's `run.started` rule carries that case.

Rules: [invariant 19](workbench-invariants.md);
[protocol.md, Approvals](../reference/protocol.md#approvals--apiv1approvals).

### 5.69 A fallback entry names a provider

Decided 2026-09-11 (workbench invariant 9).

**Decision.** `resilience.fallback_models` is a typed array of
`{provider_id, model}`: the entry runs on the provider row it names, under
the primary's reference rule (§5.29), and carries no credential of its own — a
key in an entry is `400`. A row from before the field holds the endpoint an
entry named (`provider_type`, `base_url`) and, at rest, the key it carried;
the decode drops the key, the read returns the endpoint, and the build
resolves it to a provider the agent may reference at that endpoint, with a
warning, or fails loudly. The form offers the resolved provider and drops an
entry no provider reaches.

**Rejected.** Keeping the inline key with mask round-tripping — the one
place a model key is entered was the provider (§5.30), and the agent form
asking for a raw key beside it contradicted that in the UI and in the
handler's second masking path. Refusing legacy rows outright — an agent
that ran yesterday must read and run today; only the inline key stops being
honored.

**Cost accepted.** A breaking wire change: the entry shape and the field's
type. A legacy entry whose endpoint has no provider row fails the run until
the operator adds one; its stored key is inert until the next save rewrites
the field.

Rules: [invariant 9](workbench-invariants.md);
[protocol.md, Agents](../reference/protocol.md#agents--apiv1agents).

### 5.70 An E2B port is an address to copy, not a proxy

Decided 2026-09-14 (workbench invariant 53).

**Decision.** On a service speaking the E2B API every sandbox port is already
public at `<port>-<sandbox id>.<domain>`, so the workbench shows that address
— the id and the domain the service returned, `<port>` left to the reader —
in a dialog to copy. Nothing is proxied, granted or published, and the read
neither creates nor resumes the sandbox. The row declares it through
`supports.public_host`; docker, whose ports are not public, declares nothing
and offers nothing.

**Rejected.** A port input in the menu — the port is the server's inside the
sandbox, which the person knows and the workbench does not. Reviving the port
preview (§5.35) for e2b — its cost was the gateway, which this needs none of.
Persisting the domain beside `instance_ref` — a schema column for a fact one
GET returns.

**Cost accepted.** One control-plane GET per open. The address is what the
service publishes; reachability is its policy — a service may gate port
traffic with a token, or answer every response with
`Content-Disposition: attachment` (Bailian's gateway does, whatever the
content type), which leaves the URL to `curl` and `fetch` and takes a browser
page off the table.

Rules: [invariant 53](workbench-invariants.md);
[protocol.md, Projects](../reference/protocol.md#projects--apiv1projects).

### 5.71 A running record is confirmed through the daemon

Decided 2026-09-14; verified against Bailian.

**Decision.** `Status` trusts a record that says `paused` and a 404, and
confirms one that says `running` with a GET of the daemon's `/health`: a 5xx
from the sandbox's gateway (502 on E2B, 500 on Bailian) answers stopped, any
answer the daemon gives is running, and a transport failure is the error.
Bailian's record says `running` for a paused sandbox — after its own `pause`
returned 204, and past the `endAt` it auto-paused at — while the same
service's `?state=paused` filter and its gateway both tell the truth. The
probe is E2B's own SDK's definition of "is running".

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

Decided 2026-09-14. The agents a `spawn_task` call may name are the spawning
agent's handoff targets, plus itself.

**Decision.** `agent_name` is resolved against `Agent.Handoffs` before the
host's Resolver sees it. The model already has those names from its
`transfer_to_*` tools, so the set needs no second listing to stay in step, and
one declaration names an agent's collaborators for both shapes of delegation:
hand the conversation over, or run in the background. The workbench lists the
targets in the tool's description as well and passes the SDK the config id its
build gave each target, not the name (invariant 75).

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

Decided 2026-09-22. `OutputToInput` and `normalizeStoredInput` remove
`logprobs` from every content part of an assistant message before it goes
back as input.

**Decision.** Logprobs annotate one response's tokens; as input they carry
nothing the model reads. Observed 2026-09-21 on a gateway fronting two
upstreams: one emits `"logprobs": []` on every message, the other rejects any
input carrying the key, so a conversation that touched both died on the next
turn. The strip runs at both entry points so a history written before this
rule is scrubbed on load as well as a fresh echo; the id, status, text and
annotations ride through untouched.

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
own grants: a gated `exec_command` asks until a card of that run says `same`
or `all`, whatever the session holds. The grant is the session's too.

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
`max_tokens` stops growing and the caller sets it. Only the budget path keeps
the sampling and forced-tool-choice prechecks: what adaptive thinking refuses
is the API's to say.

Rules: [models how-to](../howto/models.md#anthropic-backend-defaults)

### 5.77 A bound reasoning block is dropped, not fatal

Decided 2026-10-03.

**Decision.** A request that already carries a thinking object also carries
`thinking.block_binding.prefix_mismatch_behavior: "drop_block"` and its beta
header: the API drops a replayed thinking block whose prefix changed, and the
adapter reports each drop as a `thinking_dropped` diagnostic.

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
sees, and the stream showed an output the store does not hold. The notice is
fixed English text. A cancelled run still saves nothing. A turn
`ShouldStopAfterTurn` ended is out of reach: its save point wrote the real
outputs before the refusal.

Rules: spec §2.5

### 5.80 A trigger's payload is framed as data, not appended to the brief

Decided 2026-10-03 (workbench invariant 85).

**Decision.** A webhook's body follows the author's brief inside an
`<external source trigger>` block whose closing tag the payload cannot write,
then one fixed line saying the block is not the person's request. A task
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
as a reload does at the stored user entry. The composer queues over REST and
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

**Decision.** The workbench owns `todo_write`: a tool of its own, on an
agent's runs when `behavior.checklist` is set, off by default, refused
while the session plans — background runs too, where it is the live progress
signal, each accepted list kept as the run's session `checklist.md`. The SDK
ships no checklist: its `middleware.Todo` lost its one consumer here.

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

Rules: [invariant 34](workbench-invariants.md), [invariant 67](workbench-invariants.md), [invariant 91](workbench-invariants.md)

### 5.83 A stateful request is not replayed into the dark, and an attempt has its own clock

Decided 2026-10-03.

**Decision.** A request carrying `PreviousResponseID` or `ConversationID`
appends to a chain the server keeps, so a retry after an ambiguous failure
(a timeout, a connection severed after the send) could land the turn twice;
such an attempt is retried only when the server answered it or the dial
failed. The retry layer carries the attempt's deadlines — `AttemptTimeout`,
released once a stream commits, and `IdleTimeout` between a stream's events —
as `context.WithCancelCause` clocks whose cause is the policy's own error, so
a timed-out attempt is retried whatever `RetryIf` says and the error a caller
sees is never mistaken for its own cancellation.

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
— the system text and the tool definitions, 16 hex characters of a SHA-256 —
into each thinking block's `encrypted_content`, behind the existing
`thinking_signature:` / `redacted_thinking:` prefix. On replay it computes the
current fingerprint and leaves out the newest block bound to another prefix
together with every block before it: a leading run of thinking may be
removed, a block in the middle may not. The host re-renders its instruction
layer every run (memories, skills, a background suffix), so the prefix moves
mid-session as a matter of course; §5.77's `drop_block` remains the net under
an account that enforces the binding without this adapter's knowledge.

**Rejected.** Storing the fingerprint on the SDK entry: a field on
`session.Entry` every provider would carry for one adapter's rule. Hashing the
whole message prefix: a block is bound to its own turn's history anyway, and
the system text and tools are what a host edits. A fingerprint on the OpenAI
side: no failure has been reproduced there.

**Cost accepted.** Blocks written before fingerprints replay no more; the
first request after the upgrade thinks from scratch. An instruction edit
costs the conversation its earlier thinking, as the API would have.

Rules: spec §2.15.

### 5.85 An agent's approvals are a mode, not a checklist

Decided 2026-10-03.

**Decision.** An agent asks in one of three modes: `never` (only the tools
its list names), `on_change` (every tool plan mode would refuse), `always`
(every tool). "A change" is plan mode's own answer, `Plan.ReadOnlySet().Admits`
over the same read-only names the build hands `Plan` — so the set the mode
asks about and the set planning denies are one set, including which MCP
tools count (invariant 89). The mode is installed per tool, on every built
agent, handoff targets and background runs included; `exec_command` keeps its
per-command gate in every mode that asks, so trusting a command still means
something. The per-tool list only ever adds a question. A new agent is
`never`; an empty mode is how older rows read, and means the same.

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
backend written outside the repository (scope §3 tells its author to
implement `session.Storage`) runs the same checks from its tests. The fake
model and the run-level assertion helpers stay in `internal/agentstest`:
§5.23 still holds for them.

**Rejected.** Leaving the suites internal: the contract in spec §2.5e2 is then
checked only for the backends in this repository, and the Redis or encrypted
store the scope points people at is written against prose. A conformance
check for atomic batch appends: no backend can prove the negative from the
outside, so it stays a documented contract.

**Cost accepted.** One more public package to keep compatible; its surface is
two functions and one struct.

Rules: spec §2.5e2.
