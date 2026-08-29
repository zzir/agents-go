# Design decisions

Decisions that have been discussed and settled, each with the reason it is what
it is. **Read the rationale before reopening one.** The section numbers are
permanent addresses — code comments cite them as `decisions §5.29`, so a number
is never reused or renumbered.

The invariants these decisions produced live in
[the spec](../reference/spec.md); what the project deliberately does not do
lives in [scope](scope.md).

---

## 5. Recorded design decisions

These have been discussed and settled. Read the rationale before reopening.

**A decision is only as good as the reason recorded under it.** Entries whose
stated reason is a citation of another codebase rather than a property of this
one get marked **🔁 reason under review**: the decision stands, but it may not
be closed by citation — re-deciding one means replacing the citation with a
reason that stands on its own, or changing the decision, and dropping the mark
in the same change. Every entry below currently carries its own reason; the
mark is the mechanism for the next time one does not.

### 5.1 Handoffs stay; graph orchestration does not replace them

A handoff is "switch agent at runtime"; a graph is "declare the topology up
front". They solve different problems. Our handoffs carry an `InputFilter` and
history folding; the equivalent in a graph model takes a lot of glue. Graph
orchestration, if it ever arrives, belongs *above* handoffs — serving task
orchestration, not replacing agent switching.

### 5.2 Names describe the thing, and renames are batched

A name earns a rename only when it misdescribes or violates a Go rule — never
to "look less like Python". `RunItem`, `RunResult`, `RunContext` and friends
read fine as Go and stay. What did not, and was renamed in the breaking
commits after v0.2.1 (the v0.3.0 window):

- `Get`-prefixed methods: `Model.GetResponse` → `Respond` (an action, so a
  verb), `ModelProvider.GetModel` → `Model` (a lookup, so the accessor form).
  The Instructions/Prompt family's `Get` methods were removed with the
  func-type change ([§5.3](#53-instructions-and-prompt-both-stay-both-are-func-types)).
- The `T`-prefixed aliases spelled a Python `TypeAlias` convention with no Go
  counterpart: `TResponseInputItem` → `InputItem`, `TResponseOutputItem` →
  `OutputItem`, `TResponseStreamEvent` → `ResponseStreamEvent` (the run-level
  event interface already owns the bare `StreamEvent` name).
- `AgentsError` stuttered; resolved by deletion in the error rework
  ([§2.10](../reference/spec.md#210-errors-and-recovery)).
- `FunctionTool` → `Tool`: with the interface gone there is only one kind of
  tool, and the qualifier distinguished nothing.

The rule that survives for the future: **a rename is a breaking change and is
batched into a window users absorb once** — this batch rode the v0.3.0
window alongside the structural collapses; the next one is the openai-go v4
bump ([§5.5b](#55b-the-wire-types-couple-our-compatibility-to-openai-gos)).

### 5.3 `Instructions` and `Prompt` both stay; both are func types

`Prompt` (a server-stored prompt template with a version and variables) is a
**Responses API capability**, not a porting artifact. The two compose: a stored
prompt provides the base, instructions append to it.

Their shape: `Instructions` and `PromptProvider` are **func types**, not
interfaces. As single-method interfaces their only implementations were
unexported types in this package behind adapter constructors
(`InstructionsFunc`, `PromptFunc`) — a plug point nothing ever plugged into. A
func type is the same capability assigned directly; `StaticInstructions` /
`StaticPrompt` cover the fixed case and `WrapInstructions` composes. The
`Agent.GetSystemPrompt` / `Agent.GetPrompt` forwarders became unexported
resolution points — resolution (nil handling, prompt-ID validation) is the
runner's job, not API surface.

The same rule collapsed `tasks.AgentResolver`, `tasks.Launcher`,
`tasks.Stopper` and `tasks.WakeGuard`: each was a single-method interface with
a `...Func` adapter nobody used in production — hosts assigned method values
anyway, and a method value satisfies a func type just as directly. A
single-method injection point is a func type unless a second method is already
in sight; `tasks.Store` (multi-method) keeps being an interface.

### 5.4 A tool is a struct, not an interface

`*Tool` is the tool type. There is no `Tool` interface, which is how the
"no hosted tools" decision ([§1.2](scope.md#12-non-goals)) is enforced: a provider-hosted
tool has nowhere to be introduced, because there is nothing to implement.

This replaced a sealed interface with an unexported marker method. The seal was
doing the same job, but it also invited a wrapper hierarchy to carry optional
behavior, and that hierarchy needed a lookup protocol
([§2.7c](../reference/spec.md#27c-tool-capabilities-are-fields)) to be usable. A struct closes the
kind and carries the behavior in one move; behavior stays open because the
fields are exported and a variant is a copy.

### 5.5 Internal item types are Responses wire types

Zero conversion, zero information loss — reasoning ids, `encrypted_content` and
strict schemas all survive round-trips. The cost is that non-LLM entries need a
`session.Entry` wrapper to have somewhere to live.

### 5.5b The wire types couple our compatibility to openai-go's

§5.5's zero-conversion choice has a price with a name: `InputItem` and
friends are **type aliases of `openai-go/v3` union types**, and they appear in
nearly every exported signature. A major-version bump of openai-go (v3→v4) is
therefore a breaking change of this SDK's **entire API surface**, whatever else
it contains.

This is accepted, not overlooked:

- Wrapping the wire types behind our own structs would cost the round-trip
  fidelity §5.5 exists for, plus a conversion layer that must chase every
  Responses API addition forever.
- The major version is pinned in `go.mod`; nothing forces a bump on users
  until we take one deliberately.
- **When a bump does come, it is the merge window** for every other
  API-surface change on the shelf, so users absorb one deprecation cycle
  (§5.8), not two. (The `T`-prefix renames once parked here were taken in
  the breaking commits after v0.2.1 instead — the v0.3.0 window was already
  breaking these exact signatures.)

### 5.6 Background work runs in-process, not in isolated processes

Background sub-agents ("tasks") run as **nested runs inside the same process**,
each with its own hidden session, reporting back by injecting a notification
message into the parent session.

The alternative — supervising one OS process per session and talking to it over
a line protocol — was considered and **rejected**. It buys crash isolation and
independent working directories at the cost of IPC, serialization, and a second
lifecycle to manage. Nested runs already give us independent sessions and
configuration; the isolation is not worth the machinery at this scale.

### 5.6b Tracing stays vendor-neutral; OTel export is the consumer's job

The core `tracing` package has no dependencies: a span is a flat record with
string ids and a `Data` map. Two invariants keep that record portable:

- **`tracing.NewSpanID` is 8 bytes and `NewTraceID` is 16** — the OTel widths.
  Widening either would force every OTel-shaped consumer to truncate, silently
  and inconsistently.
- **A trace has one root span per agent**, not one per trace: a handoff
  finishes the current agent span and opens the next one under the same
  (empty) top-level parent, so an N-handoff run contributes N+1 parentless
  spans. An exporter that groups by trace must carry workflow metadata across
  roots itself.

Making the core emit OTel spans directly was rejected — a heavy, fast-moving
dependency in every consumer's build for a feature most do not use. The
`tracing/otel` exporter submodule that once did the translation was **removed**
(2026-08) with the rest of the zero-consumer surface (§5.23): the workbench
reads spans through its own store, and an external consumer integrates by
implementing a `tracing.Processor` against the flat record.

### 5.7 A submodule exists only to keep a heavy dependency out of the core

The repository is a Go workspace with a root module (the SDK) plus submodules.
The **only** reason to split something into its own module is that it would
otherwise pull a heavy dependency into the core. Test helpers, small utilities
and anything dependency-free stay in the root module regardless of how
self-contained they are.

`mcp` is a module for that reason and no other: `modelcontextprotocol/go-sdk`
brought a raft of indirect requirements with it (uritemplate, `x/oauth2`,
`x/time`, `x/sys` and the segmentio pair among them), taxing every build that
never speaks MCP. The core does not import it — `agents.MCPServer` is the
inversion that lets an `Agent` hold servers without the dependency — so the
split cost one `go.mod` and moved no import path.

### 5.8 Public API compatibility begins at v1.0.0

A minor release before v1.0.0 may break exported identifiers. Each one is
recorded in the release notes with the old spelling beside the new, and they are
batched into as few releases as the work allows, so a user absorbs one migration
rather than a drip.

**This section used to promise a deprecation cycle from v0.2.0 onward, and the
promise was not kept**: the breaking commits after v0.2.1 — the tool and item
collapses, the naming batch, the `agents/session` split — each renamed or
removed outright. Keeping a rule nobody follows is worse than not having it,
because it teaches the next reader that this document describes intentions
rather than behavior. The API is still finding its shape; the deprecation cycle
begins when it stops, at v1.0.0.

### 5.9 A parent-linked checkpoint chain for execution state is declined

Microsoft's agent-framework-go checkpoints every workflow superstep into a
parent-linked store (`CreateCheckpoint(..., parent)` /
`RetrieveIndex(withParent)`), so a run can resume from **any** historical
point and the checkpoints form a browsable tree — time-travel debugging
included. It needs that structure because its `Session` is a key-value bag:
the checkpoint tree is its only history.

Declined here, because this SDK already carries the stronger halves of that
design:

- **The session tree is the parent chain** (§2.5d). "Re-run from message X"
  and "same history, different model or options from turn N" are session
  branches: a new run starts from any leaf, its model input rebuilt by
  projection.
- **`RunState` serializes the one state that cannot be rebuilt** — the
  mid-turn pause with tool calls awaiting approval (§2.7) — and resumes
  across processes. Between turns, the session is the truth; the rest of the
  runner's state is derivable or expendable.
- **Per-turn persistence bounds crash loss to the in-flight turn**, and
  repair (§2.5h) makes the session loadable again.

The net capability a chain would add — deterministic replay, and a byte-exact
"resume turn N with the execution state it had then" — does not justify a
second history structure beside the tree, with its own consistency rules
against it.

Revisit only with a concrete replay/debugger need, and then on three terms: a
checkpoint is a **session entry kind** (payload: a trimmed `RunState`,
projected to nothing), so the tree stays the only history structure; a
deterministic execution mode comes first, because replaying a
nondeterministic run replays into different behavior; and the payload must be
trimmed — `RunState` carries every raw response, and a per-turn copy of that
grows quadratically.

### 5.10 Non-Responses backends adapt at the model boundary

The canonical item and event format stays the Responses wire format (§5.5)
even when the backend speaks something else. An adapter translates in both
directions **inside its own package** — `models/anthropic` for the Messages
API — so the runner, sessions, run state and the server never learn a second
format. `models/modelkit` (root module) holds the shared halves: the input
walker, item/event synthesizers that stamp round-trippable raw JSON, and the
feature-rejection helper.

The runner's consumption contract, which every `agents.Model` implementation
in this repository must satisfy (enforced by `modelkit/conformancetest`; both
in-repo providers run it):

- **Output items** are canonical items whose `RawJSON()` is non-empty wire
  JSON — `agents.OutputToInput` and session persistence depend on it. The
  types the runner models are `message` / `reasoning` / `function_call`;
  anything else rides through as an `ItemUnknown` run item.
- **Stream vocabulary** is `response.*` only. The first event is
  `response.created`; each finished item gets one `response.output_item.done`
  (in order); the terminal event is `response.completed` or
  `response.incomplete` — reason `max_output_tokens` is the one recoverable
  truncation (§2.7e). Text streams as `response.output_text.delta`, raw
  reasoning text as `response.reasoning_text.delta`. These names are
  load-bearing: the agents-server UI renders exactly these events. They are
  spelled ONCE, as the exported `agents.Event*` constants: the runner's
  classifiers, `modelkit`'s event constructors, the OpenAI adapter's
  terminal-event switch and `conformancetest`'s closed set all build from that
  one list, so a misspelled reference is a compile error rather than a branch
  that silently never fires. `agents/stream_events_test.go` is the one place
  that restates the wire strings by hand and pins the constants to them.
  `response.queued` belongs to the vocabulary — lifecycle preamble the runner
  tolerates wherever `response.created` / `response.in_progress` appear — but
  only a pass-through backend emits it: a synthesized stream has no queue to
  report, so `modelkit` offers no constructor for it and the conformance closed
  set deliberately leaves it out.
- **Usage** is Responses semantics: `InputTokens` is the TOTAL input count,
  cache reads and writes included; `CachedTokens` / `CacheWriteTokens` are
  informational subsets. A backend that reports uncached input separately
  (Anthropic) adds the parts.
- **Unsupported request features fail loudly** — a `*agents.UserError` naming
  the feature (`modelkit.Reject`), never a silently dropped setting.
- **Continuity blobs** (thinking signatures, redacted reasoning) ride in the
  reasoning item's `encrypted_content` — the one canonical slot that survives
  `OutputToInput` and session storage. A reasoning item without one is
  dropped on replay to a backend that requires signatures.

Anthropic-specific mappings recorded with the adapter: mid-history
system/developer messages travel as `mid_conv_system` blocks in system turns
(the Messages API has no plain `system` role for input text; top-of-run
instructions use the top-level `system` parameter); `thinking` ↔ `reasoning`,
with the blob in `encrypted_content` carrying an adapter prefix
(`thinking_signature:` / `redacted_thinking:`) — a blob without a recognized
prefix is another provider's reasoning and is dropped on replay rather than
sent as a bogus signature; `stop_reason: max_tokens` →
`incomplete`/`max_output_tokens`; `stop_reason: refusal` → ONE canonical
refusal message and nothing else (the response's text, else
`stop_details.explanation`, else a fixed line — never empty): the Messages
API reports refusal out-of-band, and a refused response's partially
generated `tool_use` blocks must not survive into items the runner would
execute before it ever looks for the refusal — so `ModelRefusalError` and
`model_refusal` handlers fire exactly as on any backend (a streamed
refusal's mid-stream `item.done` events may still show text/tool items;
the terminal rebuild is what the runner reads);
`model_context_window_exceeded` → an error carrying that marker (§2.5g);
`Reasoning.Effort` → thinking budgets (minimal 1024 / low 4096 / medium
16384 / high 32768) with `MaxTokens` defaulting to 8192 (grown to
budget + 8192 when the budget would not fit under it), and thinking rejects
`Temperature`/`TopP`/forced tool choice up front; prompt caching is the
request-level `cache_control` marker, on by default
(`Provider.WithPromptCaching(false)` opts out). `models/anthropic` is a
submodule per §5.7 — it carries the anthropic-sdk-go dependency; `modelkit`
adds none, so it stays in root.

### 5.11 Construction errors split by data provenance

A constructor whose failure can only be a programmer error **panics**; a
constructor whose input is runtime data **returns an error**.

`NewTool` and `AgentAsTool` derive their schema from a Go type: for a
given type the outcome is deterministic, so a failure (non-struct args, a field
no schema can express) is a bug that any test constructing the agent surfaces
immediately — the `regexp.MustCompile` precedent. They panic, which also keeps
constructors chainable inside `Agent{Tools: []Tool{...}}` literals.
`NewRawTool` takes a schema that is data (loaded from a database or
config), so a bad schema is an expected input, not a bug: it returns
`(*Tool, error)`.

One of those failures is a shape rather than a bug: strict mode cannot express
an `any`/`interface{}` field or a map with arbitrary keys **at all**.
`Tool.NonStrict` does not rescue it — it relaxes a tool that already exists,
while the strict schema is generated during construction — so `NewTool` has a
non-strict twin, `NewToolNonStrict`, mirroring the `OutputType` /
`OutputTypeNonStrict` pair. `AgentAsTool` has no such twin: its schema is
hard-wired strict. That is a recorded gap, not a decision — no caller has
needed an unconstrained field in a nested run's arguments yet, and until one
does the way out is building the `Tool` value directly. The normalization
errors phrase their advice accordingly: they say to turn strict off *where the
schema was built*, and name the constructors only as the Go-type example,
because the same message is reached from `NewRawTool` and
`NewDynamicOutputSchema`, where the switch is elsewhere.

The earlier design — returning a tool that errors on every invocation, surfaced
by the runner before the first model call — deferred a deterministic bug to
runtime and cost a field (`constructionErr`) plus a runner check. Rejected.

### 5.12 One user-context entry point

`RunOptions.Context` is the only way user data enters a run; every run wraps it
in a fresh `RunContext`. There is no field to inject a pre-built `RunContext`:
nothing in the SDK needed it (nested runs share the parent's `Context` value
and fresh accumulators), and cross-run usage totals are sums over each
`RunResult.Usage`. Two fields expressing one concept was the cost; a run owning
its `RunContext` outright is what the guarantee "a run's accumulators start
empty" rests on.

### 5.13 AgentToolConfig configures the tool, ModifyRunOptions the run

`AgentToolConfig` holds only what has no `RunOptions` counterpart: the tool's
name, description, visibility, approval gate, error rendering, output
extraction, streaming callback and input rendering. Everything about the
nested run itself — session, turn budget, conversation, model, guardrails —
goes through the single `ModifyRunOptions` channel. Mirror fields
(`MaxTurns`, `Session`, `ConversationID`) were removed: each was a second
spelling of a `RunOptions` field, and the escape hatch's existence proved the
dedicated-field approach could never be complete. A `ConversationID` set via
`ModifyRunOptions` is still cleared when a paused nested run resumes (the
serialized state already carries the conversation).

### 5.14 Sandbox file tools share exec's path view

The sandbox file operations (`ReadFile`, `WriteFile`, `CreateExclusive`,
`ListDir`, `RemoveFile`, `Rename`) resolve paths with **shell semantics,
identical to `exec_command`**: a relative path resolves under the working
directory, an absolute path is used as-is. The isolation boundary is the
sandbox itself, not the working directory — for local and docker-persistent,
exec already reaches everything on that filesystem, so
pinning the file tools inside `WorkDir` adds no protection; it only creates a
second path universe. The model learns real absolute paths from exec output
(`pwd`, `ls`, `git status`) and echoes them into the file tools, so the two
surfaces sharing one view is what makes those calls work. (An earlier
workdir-rooted "virtual chroot" design was dropped for exactly that failure:
absolute paths got re-joined under `WorkDir` and read as "not found".)
`ReadFile` behaves like the OS everywhere: it follows symlinks to the file
they name (bounded hops) and fails on a directory with an is-a-directory
error — docker's copy-out API does neither itself, so that backend normalizes
the raw tar semantics rather than exposing daemon quirks.

**The one exception is docker bind-mount mode**, where file operations run on
the *host* side of the mount while exec runs inside the container — the
container's isolation does not cover them. There they are confined to
`WorkDir` via `os.Root` (which also polices `..` and symlink escapes);
absolute paths must lie under the in-container mount point (`/workspace`, the
only view the model ever sees) and are translated to their host-side names,
and anything else fails with `sandbox.ErrOutsideWorkDir` — an explicit
"outside the working directory" to the model, never a silent re-rooting.

Docker's working directory may be narrowed to a **subtree of the mount**
(`Options.ContainerWorkDir`, `/workspace` by default, validated by `New` to be
`/workspace` or below it): the mount point never moves, but commands run — and
relative paths in the file tools resolve — in that subdirectory, exec and file
tools moving together per this section's rule. Absolute `/workspace/...` paths
keep addressing the whole mount, exactly like a shell `cd`'d into the subtree
still can.

### 5.15 Streaming-only backends adapt with a Model decorator

Some backends accept **only** streaming requests — the ChatGPT Codex backend
(`chatgpt.com/backend-api/codex`) rejects a non-streaming POST with 400. The
adaptation is `NewStreamOnlyModel` / `NewStreamOnlyProvider`: a
provider-agnostic decorator whose `Respond` runs the request as an
internal `StreamResponse` and assembles the final `ModelResponse` from the
terminal event; `StreamResponse` passes through untouched.

It is a **Model decorator, not an HTTP middleware**, because forcing
`"stream": true` at the transport layer would hand an SSE body to a caller
that parses a JSON response — the request shape and the response parser must
switch together, which only the model boundary sees. Assembly is shared with
the runner's own streaming path (one `responseAssembler`), so the two paths
cannot drift; like that path, the assembled response carries no `RequestID`
and treats a length-truncated `response.incomplete` as an arrived (not
failed) response. Compose it **innermost**, directly on the backend it
adapts: decorators above it (retry, fallback, routing) then see a severed
stream as an ordinary `Respond` error and handle it normally.

### 5.16 A severed stream retries only before output, with the preamble held back

Three rules govern a stream that dies before its terminal event:

- **Classification**: `modelkit.RetryableError` treats `io.ErrUnexpectedEOF`
  as a transport failure (retryable), alongside `net.Error` — a gateway or
  proxy severed the connection mid-event.
- **Truncation is an adapter obligation.** A connection severed *at* an event
  boundary looks like a clean EOF to the SSE layer — no transport error, the
  stream just ends. An adapter must not let that pass as a normal finish:
  when the stream ends cleanly without a terminal event
  (`response.completed` / length-`incomplete`; `message_stop` on Anthropic),
  it surfaces `modelkit.TruncatedStreamError`, which wraps
  `io.ErrUnexpectedEOF` so the classification above applies. Without this the
  failure would fall through to the runner's "ended without a completed
  response" — accurate, but unretryable. (The runner keeps that check as the
  last line of defense for models that don't.) Symmetrically, a transport
  error AFTER the terminal event is not surfaced: the response is complete
  and delivered, and failing the call then would throw away a valid result
  over a connection with nothing left to say.
- **Commit window, with pre-commit events held back**: `NewRetryModel` and
  `NewFallbackModel` may replace a broken attempt only while nothing has been
  **generated**. Two event classes carry nothing the model generated:
  lifecycle preamble (`response.created`, `response.in_progress`,
  `response.queued`), which arrives the moment the connection opens, and
  terminal-failure events (`error`, `response.error`, `response.failed`) —
  replacing an attempt that ends in one of those is the whole point, and the
  streaming chain must advance on a `response.failed` exactly like the
  blocking chain advances on the error it becomes. (`response.incomplete` is
  NOT in this class: a length-truncated response is output that arrived, and
  committing on it is what stops a retry from throwing it away.) Once
  delivered, though, such an event would commit the consumer to a response a
  second attempt then duplicates — so the decorators buffer them
  (`deliverStreamAttempt`): an abandoned attempt's pending events are dropped
  and the consumer sees exactly one coherent response, with a `model_retry`
  span and a `DiagModelRetry` diagnostic as the only trace of the failed
  attempt. Pending events are flushed when the attempt turns out to be the
  stream's last word — the first output event commits it (from there, errors
    pass through, recorded as `DiagStreamError` by every decorator that saw
  them — the retry naming the attempt it could not repeat, the fallback the
  backend it could not leave, so a nested chain accounts for one break once
  per layer), and a clean all-pending
  finish or a terminal failure delivers them before the verdict. A nil event
  neither commits nor buffers (dropped, as the runner does), and a consumer
  that stops mid-flush ends everything — no further events, no diagnostics.

---

### 5.17 The session layer is its own package

`agents/session` owns stored history: entries, storage, the semantics struct,
projection, the tree, forking, recovery, and the wire codec
(`MarshalInputItem` / `UnmarshalInputItem` — their consumers are exactly the
storage implementations). The runner imports session; session never imports
the runner. Its one upward need — building an entry from a live `RunItem` —
stays in agents as `EntryFromRunItem`.

Names inside dropped their `Session` prefix (`session.Entry`,
`session.Storage`, `session.Repo`, `session.Recover`, `session.Fork`,
`session.ErrNotFound`); `session.Session` keeps the stutter the way
`context.Context` does, because the concept IS the package.

The value types shared by both layers — `Source`, `ItemDisplay`,
`RequestUsage`, `Diagnostic`, `ErrorCode` — live in session (entries persist
them) and are **aliased** in agents (`agents.Source = session.Source`),
because they are equally part of the runner's surface: every `RunItem` carries
a `Source`, every result reports `RequestUsage`. An alias is transparent — one
type, two import paths — so neither layer's API is second-class. **Each alias
keeps the name it aliases.** A renamed alias stops being transparent the
moment anything spells the type out: the compile error, the godoc and the
reflected name all say the session name while the code says the agents one.
(`agents.ItemDisplay = session.Display` was the one that drifted; session's
type was renamed to `ItemDisplay` to close it, which is also the more accurate
name there — what it projects is an item.) `ErrorCode`
specifically: the *vocabulary* sits in session because entries and
diagnostics persist codes; the *derivation* (`CodeOf`, `Classify`) stays in
agents with the error types it reads.

Session-only names are deliberately NOT aliased: code that works with stored
history imports the package that owns it. This was the §6.4 split, taken
after the structural collapses so the code moved once.

---

### 5.18 A RunState decodes across a version window, and the window is earned

`RunStateFromJSON` accepts the same schema major from
`runStateOldestDecodableMinor` up to `RunStateSchemaVersion`, rather than
demanding strict equality. The reason is what a pause IS: an approval waits on
a human, the process may be redeployed while they decide, and refusing the
state afterwards strands the run for a reason the user had no part in. The
field-by-field fallbacks the decoder already carries — a zero `MaxTurns`
meaning `DefaultMaxTurns`, `UsagePending *bool` separating absent from false,
an absent cursor meaning zero — are what make an older minor readable; under
strict equality they were cost with no payer.

The window is not free and it is not retroactive. **A minor may only ADD
fields.** A bump that REPLACES or reinterprets one must raise
`runStateOldestDecodableMinor` to itself, because such a state decodes
*successfully* with its old fields silently dropped — strictly worse than a
refusal, since the caller is told the resume is faithful.

The floor sits at 4 and does not reach back further: `"1.3"` was stamped by
released builds both before and after the four guardrail-result keys collapsed
into a single `guardrail_results`, so two incompatible payloads share that one
string, and accepting it would drop every recorded guardrail result from the
older shape — resume is the only path that carries first-turn input-guardrail
results forward at all. The bumps since have been purely additive (1.5 the
off-chain-history flag, 1.6 the host extra map), so the window is now real: a
1.4 state decodes under a 1.6 SDK.

A consumer triaging stored states must apply the same window, via
`RunStateVersionSupported`, never string equality against
`RunStateSchemaVersion` — an equality gate destroys states an additive bump
resumes fine (agents-server's approval pre-flight did exactly that until it
switched).

`RunState.Extra` (1.6) is host-owned state riding the pause: the SDK marshals
and unmarshals it verbatim and never reads a key. It exists because a
build-time agent transform (`middleware.Plan.Apply`) returns fresh state on
rebuild, so what the transform knew — a plan phase's unlock — must travel with
the pause or the host invents a side channel. It covers pause→resume only: a
fact that must survive a crash mid-run needs the host's own durable write at
the moment it happens (`PlanPhase.OnUnlock`), and the two records answer
different questions.

### 5.19 A named container is adopted only against a configuration fingerprint

A persistent docker sandbox with a fixed `ContainerName` can find the name
already taken — by a container a previous process run left behind, or by
something else entirely. Adoption (taking the existing container over instead
of erroring) is allowed **only when the container proves to be ours from the
same configuration**: creation stamps a label carrying a hash of every
security-relevant option (image, runtime, user, network, bind source, resource
limits), and `adoptNamed` requires an exact match.

Matching on image + mount alone — the original rule — was a hole: a container
created under a laxer policy (network on, root user, no limits) passed both
checks and silently served a config that no longer allows any of it. The
fingerprint hashes **effective** values (the resolved user, the applied PIDs
default), so equivalent spellings of one configuration still adopt.
`ContainerWorkDir` is excluded on purpose — persistent mode passes the working
directory per exec, so it does not change what the container *is*. A container
without the label (foreign) is a hard error naming the remedy: remove or
rename it.

Revised 2026-08-24 with derived container names (§5.28): a container that
carries our label but a DIFFERENT fingerprint is ours from an older
configuration — a config edit since it was created — and is **replaced**
(removed, recreated) instead of erroring. With `KeepOnClose` the containers
outlive the process and every config edit would otherwise strand its old
container on the derived name forever. The ownership rule is unchanged: only
the label decides ours-vs-foreign, and a foreign holder is never touched.

### 5.20 A shared connection is not a caller's to cancel

An MCP session is shared by everyone configured with that server — several
runs, their background tasks, other conversations — while a run's context
belongs to one of them. **A request on a shared connection therefore rides the
connection's context, not the caller's**, and the caller's cancellation is
honored by returning from the wait rather than by cancelling the request. The
answer that arrives afterwards is dropped.

The alternative was tried and cost a great deal: the streamable HTTP transport
issues each request on the context it is handed, and one cancelled mid-flight
makes the go-sdk fail the whole CONNECTION — a `sync.Once` closing its failure
gate. Every later call by anyone then answers "client is closing" until
something reconnects, which nothing does. One person stopping one run was
observed failing five background tasks across two conversations inside seven
seconds, each blamed on its own agent's MCP server rather than on the stop that
actually did it.

The price is one in-flight request outliving its caller, bounded by the
connection's own lifetime (`Close` ends it). That is the right trade against
a connection outage for every other user of the server. The rule generalizes:
**a resource shared between runs may not be handed a single run's
cancellation** — a per-run deadline on a per-process resource is a way for one
run to break another.

### 5.21 A dead shared connection repairs itself, and a tool call is not repeated

Isolation keeps one caller from killing the connection; it does not make
connections immortal. A server restarts, a proxy drops an idle socket. Nothing
in the go-sdk reconnects, so **the connection owns its own recovery**: given a
way to rebuild its transport (`mcp.Options.Redial`), a session found dead is
replaced in place. In place matters — every holder of that server recovers,
not only the runs that start afterwards, which is the difference between one
task failing and every task failing.

Three bounds make it safe. **A death is noticed as it happens**, by watching
the connection rather than waiting for a caller to trip over it, because the
callers who would pay are whoever is mid-run. **Healing is throttled**, so a
server that accepts a connection and drops it again cannot become a dial loop.
And **only idempotent work is repeated**: `tools/list` is re-issued on the
fresh session, while a failed tool CALL is reported to the model rather than
retried — a dead connection cannot say whether the server ran that tool before
the line dropped, and running a write twice is worse than reporting it once.

Recovery is opt-in because only the owner of the configuration can rebuild a
transport: an `*exec.Cmd` is spent once, an endpoint needs its headers, proxy
and OAuth handler. Without `Redial` the old behavior stands — the failure is
reported, not repaired.

### 5.21b An MCP retry waits on the transport, never on an answer

`MaxRetryAttempts` retries a failed `list_tools` / `call_tool`. What that may
wait for is bounded twice.

**By kind.** A transport failure is worth another attempt: each one reloads the
session, so a connection the watcher healed in the background ([§5.21](#521-a-dead-shared-connection-repairs-itself-and-a-tool-call-is-not-repeated))
carries the next try. An answer the server *sent* is not — a JSON-RPC parse
error, invalid request, unknown method, invalid params, or the transport's own
"rejected" all mean it understood the request and refused it, and the same
bytes earn the same refusal. Neither is a call made after `Close`: no amount
of waiting turns a closed server into a live connection.

**By time.** The delay doubles per attempt but is capped at 30s and jittered
into `[d/2, d]` — the same cap and the same equal jitter as the model layer's
`RetryPolicy`, so a server shared by many runs is not retried in lockstep. The
cap is what keeps the exponent honest: uncapped, a one-second base is sleeping
half an hour by the twelfth attempt, and the run is indistinguishable from
hung long before the exponent could overflow.

The two bounds are what let `-1` stay a real setting rather than a footgun:
"indefinitely" means one attempt every 30s until the caller's context ends,
and the errors that could never succeed leave on the first try.

Sharing the model layer's `RetryPolicy` was rejected. The timing is the part
worth matching, and it is matched; the **classification** is the part that
differs, and `DefaultRetryIf` — retry everything except context cancellation —
is exactly the policy that made an infinite MCP retry indistinguishable from a
hang. One knob with two different defaults is an abstraction serving neither.

### 5.22 Retry policy lives in one layer

Both official clients retry transient failures on their own (2 attempts by
default), and `agents.NewRetryModel` wraps the whole model call from above; the
two layers compose multiplicatively, and neither can see the other. So
`openai.NewProvider` and `anthropic.NewProvider` build their clients with
`WithMaxRetries(0)`: the SDK's one retry layer is `NewRetryModel`, which is
provider-agnostic, classifiable (`RetryIf`), and observable (a span per
attempt). A provider used without it performs no retries. The transport layer
is re-enabled, not forbidden — the caller's own `option.WithMaxRetries` is
appended after the default and overrides it.

A server-suggested `Retry-After` longer than the configured `MaxDelay` **ends**
the retries — returning that attempt's wrapped error — rather than clamping to
the cap and trying again: a wait the caller capped below what the server asked
for is a signal to stop, not to retry sooner.

### 5.23 Zero-consumer surface was cut to the workbench's actual needs

Applied 2026-08, on the standing rule that a feature nothing consumes is
removed (§1.2). Four cuts in one pass:

- **`tracing/otel` removed** (with `examples/otel`). The workbench reads spans
  through its own store; the integration seam for anyone else is
  `tracing.Processor` (§5.6b).
- **`filesession` removed.** Nothing imported it but its own tests. Durable
  sessions are the `sessions` module (SQL) or the server's store; the
  zero-dependency option in core is `session.NewInMemorySession`.
- **`agentstest` is `internal/agentstest`** (with `examples/testing` and
  docs/testing.md's public-harness framing gone). The harness and the four
  conformance suites stay, as test infrastructure rather than API: the
  repo's own test files and the nested modules (path-prefix rule) keep
  importing it. Testing user code against the SDK means implementing
  `agents.Model` — the seam is the API, not a shipped fake.
- **`tools/bravesearch` is gone** (with `examples/bravesearch`): web search
  first left the SDK for the workbench in this pass, and the workbench then
  retired it too (2026-08-24) — a model-side web tool belongs to an MCP
  server the operator configures, not a hard-coded vendor integration.

`cmd/verifydocs` and `cmd/verifyexamples` merged into `cmd/verify` in the same
pass — one CI step, two checks — and **`cmd/verify` was removed entirely**
(2026-08-26). Documentation is kept correct by syncing it inside the change
that moved the code, not by a checker run afterwards: a name check passes
snippets that no longer compile, and the attempt to close that gap by
compiling them wanted a wrapper, an import table and a per-snippet opt-in —
machinery to maintain in place of the discipline it was standing in for.
Removing it also gives up running every example against fake model APIs on
each build; `go vet ./...` and `go build ./...` still compile them, so what is
lost is catching an example that compiles and then panics or hangs. A follow-up cut removed the whole MCP
serve direction — `NewAgentServer` (nothing consumed it, not even the
example), then `NewToolServer`/`ServeStdio` with `examples/mcpserver` (the
example was their only consumer). The `mcp` module is a client; the workbench
serves its own MCP endpoint independently.

### 5.24 The workbench has no provider routes

Removed 2026-08-24. The server once mapped model-name prefixes to providers
(`provider_routes` + a `RouterProvider` over every run). Two selector surfaces
for one decision — the agent's `provider_id` and a global prefix table that
silently overrode it — is one too many: an agent names its provider, full
stop, and cross-provider mixing inside one run is fallback entries
(`fallback_models`), which are per-agent and visible in its config. The SDK's
`RouterProvider` decorator stays (it is documented public API for embedders);
the workbench just no longer builds one. Do not reintroduce a global routing
table — a future need for shared endpoint selection belongs on the provider
rows themselves, not a second table that redirects them.

### 5.25 The workbench speaks one MCP transport: streamable HTTP

Removed 2026-08-24. A stored stdio MCP server was arbitrary command execution
on the host as the server's own process user — a standing RCE surface behind
one admin write, and the blocker for ever letting non-admins configure MCP.
`McpServerConfig` therefore carries no transport discriminator at all:
`Config` IS an `HTTPMcpConfig`, and a field that could only ever hold one
value was removed with the feature. A local stdio-only server joins through a
stdio→HTTP proxy (mcp-proxy and the like), run and supervised by the
operator, outside the workbench's authority. The SDK's `mcp` module keeps
its stdio transport — an embedder spawning a subprocess in their own program
is their own trust decision. Do not reintroduce stdio configs on the server;
a sandboxed variant would be a new decision, argued here first.

### 5.26 A skill is one SKILL.md document

Decided 2026-08-24, a deliberate narrowing of the Agent Skills format. A
skill is the SKILL.md document alone — no bundled `scripts/`, `references/`
or `assets/`. The full format's file trees forced skills onto a filesystem;
a single document lives in the workbench's database like every other
configuration entity, and the SDK's `skills` module shrinks to storage-free
primitives: `Parse([]byte)` validates a document, `RenderIndex` renders
discovery. Consequences, all intended:

- **Activation is `read_skill(name)`** — a function tool the RenderIndex
  wording names and the caller provides (the workbench serves it from the
  skills table; an embedder from wherever their documents live). There is no
  path-based file reader any more, and no os.Root confinement to need.
- **A skill that instructs the model to run bundled scripts does not get
  them.** Imports keep only SKILL.md files; a body referencing its repo's
  other files imports as-is and those references dangle. The model can still
  follow instructions by writing its own code in the sandbox.
- **Local edits win over re-imports**: an edited imported row detaches from
  its source and is never overwritten by a re-import; imports pin one commit
  and the server never runs git. The API mechanics live in the server
  README's Skills section.
- **Import URLs are member-supplied outbound requests** (GitHub API, raw
  fetches), like provider base URLs and MCP endpoints — the absence of an
  SSRF defense is §5.29's recorded accepted risk. Each fetch is bounded by
  a 30-second timeout, connect through body read, and the whole import by
  a five-minute budget (both revised 2026-08-25) — a stalling target must
  not hold the handler's connection open indefinitely, and per-fetch bounds
  alone would let a ~200-file walk of stalling fetches stretch into hours.
  Files past an expired budget land in `skipped` with the deadline error,
  never dropped silently.

Do not add per-skill file storage back; a skill needing an artifact should
inline it or instruct the model to fetch it.

### 5.27 The workbench's sandbox is Docker, and SSH is how a remote daemon is reached

Decided 2026-08-24. The server's `local` and `ssh` sandbox types are removed:
a local sandbox was host execution behind one admin write and one approval
(and the reason a web terminal had to be special-cased off), and the generic
SSH sandbox ran commands with a login user's full privileges, no limits, on a
machine the server merely had credentials to. The removed types are gone; a
sandbox is a Docker container (this section) or, since §5.34, an
E2B-compatible one. For Docker, what varies is WHERE — `DockerConfig.Host` empty for the local
daemon, `ssh://user@host` for a remote one, `tcp://` for the exposed case.
Empty means THIS machine and its filesystem (the SDK client honors
`DOCKER_HOST` when no Host is given). The empty-Host `DOCKER_HOST` guard this
section once described is gone: it went with the bind mount that made it matter
(§5.33) — a project's storage is now a volume the containers and file tools
share by construction, wherever the daemon is.

The SSH machinery lives on inside `sandbox/docker` as a TRANSPORT: a pure-Go
dialer (x/crypto/ssh) that opens direct-streamlocal channels to the remote
`docker.sock` over one shared, self-healing connection. It requires only
sshd with streamlocal forwarding and socket access for the SSH user — no
remote docker CLI, no local ssh binary. Self-healing is for transport
failures only: a rejected channel open (a container port nothing listens on
yet) arrives on a healthy transport, and reconnecting on it would sever every
stream multiplexed on the shared client — the preview proxy probing a dev
server that has not started must not kill the terminals riding the same
connection. The `sandbox/ssh` module is deleted,
not parked: its Sandbox implementation had become the workbench's only
consumer, and an embedder who wants raw remote exec can use x/crypto/ssh
directly — the value this repo added was the sandboxing, which SSH never
provided. The SDK's `sandbox.LocalSandbox` stays (embedders and tests; the
server just never offers it). Where a sandbox runs is its identity (the
binding freeze — workbench invariant 27): changing the daemon moves every
container's filesystem, so it freezes while any project lives on it.

Revised 2026-08-28: the daemon address and the image live in one `sandboxes`
row, whose destination freezes while projects live on it — §5.36.

Do not reintroduce a host-exec sandbox type or a raw remote-exec one; an
isolation need beyond containers (VMs, gVisor) is a new backend decision
argued here first — gVisor is already reachable today via `runtime: runsc`.

### 5.28 A project is the unit of working storage, and containers are per project

Decided 2026-08-24. A **project** is one user's working tree on one sandbox:
`projects(id, owner_id, sandbox_id, name)`, name unique per (owner, sandbox)
and display-only — storage is keyed by id, so a rename moves nothing (revised
2026-08-28, §5.36: one column, not two). The machine affinity is deliberate: a
tree lives on one daemon, and a project that could "move" between daemons
would silently be two different sets of files. A session's permanent binding
is `project_id` (revised 2026-08-28, §5.33: the project pins its machine, so
the second column was derivable); the old free-form working directory is
gone — execution is
always the container's /workspace, which mounts the project's storage:

Storage is the named volume `agents-proj-<project id tail>` on the sandbox's
daemon — the same on every daemon (revised 2026-08-28, §5.33: the local
bind-mount branch is gone). Every volume name is SERVER-derived from the id
— no user-typed path ever reaches a mount, which retires the old host-side
path validation wholesale.

**Containers are persistent-only, one per project**, named
`agents-<project tail>` — deterministic, so restarts re-adopt by fingerprint
(§5.19) instead of duplicating. `KeepOnClose` stops rather than removes on
teardown: installed packages survive idle and restarts; a config edit
replaces the container via the stale-ours adoption rule. A container found
stopped — by the admin panel, a manual `docker stop`, a daemon restart — is
restarted in place; remove-and-recreate is the fallback only when the start
fails or the container is gone. Restart-by-cached-id needs no fingerprint
re-check: the id only ever came from our own create or a verified adopt, and
ids, unlike names, cannot change hands. /tmp is a tmpfs capped at 1g
(RAM-backed — size accordingly).

Deletion contracts settle in SQL, per dialect (revised 2026-08-29): on
SQLite the single writer makes the in-statement guards atomic — the bind
CAS's EXISTS on the project row, the project delete's NOT EXISTS over bound
sessions, the target delete's NOT EXISTS over project rows. On PostgreSQL,
where READ COMMITTED lets two such single-statement guards commit as write
skew, each write first locks the parent row — the bind takes FOR KEY SHARE on
the project row, the project delete FOR UPDATE on its own row, the sandbox
guarded writes FOR UPDATE on the sandbox row (the lock a project create
takes) — and re-evaluates its guard in a fresh statement under the lock.
(Revised 2026-08-28: the old cascade is gone — a project delete now reclaims
storage, so cascading one would destroy working trees as a side effect of
removing a machine.) The
project create locks both the target and the template row for the insert's
duration, so a racing delete of either arrives first and refuses the create —
never an orphan. **A project delete DESTROYS its storage** (revised
2026-08-28, §5.33). A run naming no project gets no sandbox tools at all.
Projects are the first PERSONAL configuration entity: every member manages
their own; ownership is scoped in the handlers, not the admin gate. An admin
additionally MANAGES the plane (revised 2026-08-25, §5.29's
manage-not-author line): `?all=true` lists every owner's rows and an admin
may delete any project. Listings carry each row's bound-session count;
`storage_hint` (the volume and the daemon it is on) is reported to admins
only — a daemon address is a server-side fact a member's container never
sees, so the member's delete dialog says the tree is destroyed without naming
where it was. The web
terminal follows the same line: a member opens a shell into their OWN
project's container (a foreign project reads as absent), and an admin into
any — the operator's escape hatch, and a deliberate exception to
"session content is owner-only": a shell into a member's project tree can
read files the member's runs wrote.

---

### 5.29 Configuration is scoped per row: private to its owner or global

Decided 2026-08-24, owner semantics revised 2026-08-25. The five
configuration entities members compose runs from — agent configs, providers,
MCP servers, skills, workflows — carry two independent columns: `scope ∈
{private, global}` decides **who sees** the row, and `owner_id` names **who
wrote** it. The owner is permanent: it is stamped at create, survives every
scope flip, and changes only through an explicit transfer. Every scoped row
has one — a write leaving it empty is an error, not a legacy global default.

A **global** row is readable by every member; a **private** row is its
owner's alone, and other members never see it — foreign private ids answer
404 and are absent from listings, so scope is not an existence oracle,
mirroring sessions. A create defaults to private; claiming `global` in a
create body is admin-only (403).

The write matrix follows from the two columns:

- **Edit** — the author edits what they wrote, private *or* published; an
  admin additionally edits any global row. An admin does **not** edit a
  member's private row (403): a config an admin could silently rewrite under
  a member's name would blur whose credentials and instructions a run
  carries. Deliberately accepted with this shape: a member's published row
  stays theirs to change after the admin approved it — one team, one trust
  boundary, the same stance as the SSRF note below.
- **Delete** — the author, or an admin (management, on any row).
- **Publish** (`POST /<entity>/:id/scope` with `{"scope":"global"}`) — the
  admin's alone. Publishing to every member is the review point, so it is the
  role that reviews.
- **Unpublish** (`{"scope":"private"}`) — the admin's *or the author's*. The
  row returns to its author, never to the acting admin, because the author
  never left it. A request naming the row's **current** scope is refused
  (409): a flip is defined FROM the other scope only.
- **Transfer** (`PUT /<entity>/:id/owner` with `{"user_id"}`) — the admin's
  alone, for handing a row (credential included) to another account. Scope is
  untouched; an unknown account is 400 and a name already taken in the target
  owner's namespace is 409. **A transfer re-validates the row's references AS
  THE NEW OWNER**, exactly as a save does: an agent whose provider, MCP
  server, skill or handoff target the new owner cannot see is refused (400),
  and so is a workflow whose step agents they cannot see — handing over a
  config that answers 204 and then fails every run is the state a save
  already rejects. A provider's transfer additionally carries the demote's
  guard: refused (409) while an agent outside the NEW owner's private set
  would be stranded, because moving the credential and hiding it are one
  event for a run. Skills transfer per repo group (§5.31).

Name uniqueness is **per visibility context**, by partial unique indexes:
unique among global rows, and unique per `owner_id` among private ones. So
shadowing a global name with one's own is legal, and a flip or transfer that
collides in the target namespace is 409. **Skills add the import source to
that key** — see §5.31. Everywhere a NAME resolves — `read_skill`, the
spawn/task agent lookup, workflow matching — resolution is
**own-over-global**: the caller's PRIVATE row wins over a global row of the
same name. Owning the global row is not shadowing (`store.Shadows`): an
author who published a name still gets their own private row of it, since
scope, not authorship, is what "own" means here.

Scoped listings use ONE order everywhere — the settings panels, the admin
plane, and the sets a run is built from: **global rows first, then each group
newest first** (`created_at DESC`, id the final tiebreaker). Shared rows lead
because they are what a member picks from; newest leads within a group
because what somebody just made is where they look for it. The sort key is
creation time alone, never a name or a scope, so a rename never moves a row
and a scope flip only moves it between the two groups. The skills panel groups
by repository and keeps that same order — published groups first, the rest as
the rows arrived. The skills index deduplicates by model-facing name; the
owned row's description wins outright, matching own-over-global reads. Every
authenticated member may read `GET /auth/user-labels` (id, name, email) so a
listing can say whose each row is; roles and account state stay admin-only.

References across rows split by whether the reference is load-bearing:

- **Write-time validation** where a dangling or hidden reference breaks the
  holder: an agent's provider, MCP selection, skills selection and handoffs,
  and a workflow's step agents. The rule is `RefVisible`: a private holder
  may reference global rows and its owner's private rows; a **global holder
  may reference only global rows** — otherwise promoting it would publish a
  config whose parts most members cannot see. Promote re-validates the row
  as its target scope, and demoting a provider is refused (409) while global
  or foreign-owned agents still reference it. The provider leg — the one that
  spends a credential — settles its races in SQL: an agent write locks the
  provider row and re-checks `RefVisible` in the same transaction, a
  provider demote counts foreign references and flips in one transaction, a
  scope flip lands only FROM the other scope (the same-scope 409 is a SQL
  predicate, so two racing demotes cannot both flip a row), and run-time
  resolution re-checks the rule once more, failing the run
  loudly rather than spending a key that became private. The other reference
  kinds stay validation-plus-runtime-filtering; their races strand no
  credential.
- **Runtime filtering** where the set is advisory: attaching MCP servers and
  skills at agent build drops rows the run's owner cannot see instead of
  failing the run — the same config yields each member their visible subset.

**Authorization is re-checked inside the write.** A handler authorizes
against the `(scope, owner)` pair it read, and a transfer or scope flip can
land between that read and the write. So EVERY scoped mutation carries that
pair into its write and refuses when it no longer holds (409,
`ErrOwnershipChanged`): an update compares it against the locked row
(`save_workflow` included), a scope flip and a delete carry it as a SQL
`WHERE` predicate, and a skill import re-reads its group under lock. None of
them is an edit made under a permission that is already gone, and none
writes back a stale owner.

**A write that moves a row between visibility contexts re-checks the
credential leg in its own transaction.** Promoting an agent, and transferring
one, both re-run `RefVisible` against the LOCKED provider with the pair the
row is about to hold — so a provider demote landing between a handler's
validation and the flip cannot leave a global agent on somebody's private
key. Both take the provider lock before the agent's, the order every agent
write uses, because two writes that disagree on lock order deadlock under
PostgreSQL (a test asserts the order by racing them). The advisory legs — MCP
servers, skills, handoff targets, a workflow's step agents — stay
validation-plus-runtime-filtering, as above: their races strand no
credential.

Direct DB writes (internal writers, tests) that leave scope empty land
**private** — `stampScope` in `BeforeAppendModel` applies the same default a
create does; a row without an owner is an error, whatever its scope. The API
always stamps both explicitly, so neither ever reaches the DB unset.

The model's tools follow the same contract instead of the old admin gate:
`save_workflow` rides every owner's run now — a new name saves a private
workflow owned by the run's owner; an existing **global** name is still an
admin's to change, and a member's save answers with guidance text (pick
another name), not an error. Signing a provider into ChatGPT (or out) is the
row's editability — a member connects their own provider; status follows
visibility. Triggers stay session-scoped (the README's trigger section) with
a cap of 50 per owner (409 above it) so the shared clock is not one member's
to exhaust.

Accepted risk, recorded deliberately: member-supplied URLs (MCP endpoints,
skill imports) get **no private-network/SSRF defense**. The deployment model
is one team, one trust boundary (same stance as shared sandboxes); an
operator who needs egress control applies it outside the server.

---

### 5.30 A credential lives on the row that spends it — no global fallback keys

Decided 2026-08-24, closing out §5.29: with providers per-user, a global
credential that any keyless row silently inherits is exactly the ambient
authority the scope model removed, so the settings-level credentials went
with it.

- **`openai_api_key` / `anthropic_api_key` (fallback keys) are gone.** A
  provider row (and a `fallback_models` entry) carries its own key or runs
  keyless; an agent with no `provider_id` reaches no credential and fails
  its pre-flight ("no API key configured") until it names a provider. No
  build path reads a key from settings.
- **`brave_api_key` and the `brave_search` tool are gone.** A hard-coded
  vendor tool injected into every agent off one global key predates MCP;
  web search is an MCP server the operator (or member) configures.
- **`github_token` is gone.** Skill imports call the GitHub API anonymously
  — two requests per import keeps the anonymous limit comfortable, and
  private repositories are simply not reachable. A stored token spendable
  by any member's import was the same ambient-credential shape as above.

Settings retain the secret MACHINERY (KindSecret: masked reads, mask-echo
writes, sealed at rest) with zero secret keys registered — it is the
registry contract that stops the next credential setting from shipping
unmasked, not a per-key feature. Rows left in older databases under the
removed keys list as `unknown` and are deletable from the panel.

---

### 5.31 A skill's identity carries its repository; a repo publishes as one group

Decided 2026-08-25, refining §5.29 for skills alone. An import lands a whole
repository's `SKILL.md` files at once, and two repositories may each ship a
`review`. Two consequences:

**The repo is part of the name.** A skill's model-facing name is
`<repo label>:<frontmatter name>` — `owner/repo` for a `github.com` source,
the host for any other URL, and no prefix at all for a skill authored in the
workbench. The rendered skills index and `read_skill` both use that qualified
name, so a model naming `anthropics/skills:pdf` cannot reach a different
repo's `pdf`. Uniqueness follows the same key: unique on
`(source_repo, name)` among global rows and on `(owner_id, source_repo,
name)` among private ones. Importing a repo whose skill shares a name with a
local one is therefore not a collision — both exist, under different names —
while two files of ONE repo claiming one name still is (the second is
skipped with a reason, the rest of the import proceeds).

The label is **materialized on the row** (`repo_label`, derived from
`source_repo` on every write) because it is what the indexes key on: two
source URLs can reduce to one label — a repo walk of
`https://github.com/o/r` and a raw import of a blob URL under it both label
`o/r` — and a duplicate qualified name would make `read_skill`'s answer a
coin flip. Keying on the raw URL would let that pair through.

**A repo group is one scope, and one owner.** Both move per
`(source_repo, owner_id)` group: scope through `POST /skill-repos/scope` with
`{repo, scope}`, ownership through `PUT /skills/:id/owner` on any of its rows.
Each is one SQL statement, all or nothing, so a name taken in the target
namespace fails the whole move (409) rather than leaving the group split. A
transfer into an owner who ALREADY holds a group for that repository is
refused (409): merging two groups is how a mixed-scope pile would form, and
the unique indexes cannot see it — they partition BY scope, so a global row
and a private one never collide.

**Every operation on a group NAMES it** — `(repo, owner)`, the owner
defaulting to the caller — rather than searching for a plausible one. A sync
(`POST /skill-imports` with `{url, owner_id?}`) refreshes exactly the named
group: naming another owner is an admin's act (403 for a member) against
that owner's **published** group — a member's private group is not an
admin's to write, so it answers 404 exactly as the row reads elsewhere, and
so does a group that does not exist (a first import may only create the
caller's own). Without the naming, an admin holding a private copy of a
repository somebody else published would refresh their own copy while
believing they synced the published one.

**An import fetches everything, then writes it in one transaction.** The
fetch is up to the whole import budget — minutes of network — and the group
could be transferred, emptied or flipped inside that window. So nothing is
written during it: the documents are collected (bounded by a total-size
budget as well as the per-document cap), and `ApplyImport` then re-reads the
group under lock, refuses the whole apply when its `(owner, scope)` no
longer matches what the caller resolved before fetching (409, nothing
written), and lands every document against that one reading. That is what
makes the group an aggregate without being a table: the invariant needs one
consistent instant, not a persisted root, because a group's identity is
`(source_repo, owner_id)` — data the rows already carry — and only the WRITE
needs to be serialized.
`POST /skills/:id/scope` refuses an imported skill (400) and serves only
workbench-authored rows, which are each their own group. Authorization is
§5.29's: publish is the admin's, unpublish the admin's or the group owner's,
transfer the admin's. A later sync's NEW files inherit the group's scope and
owner instead of landing private to whoever ran the sync — otherwise a
published repo would split itself on every upstream addition. **The
consequence is deliberate**: the author of a published repo can therefore add
global skills by pushing upstream and syncing, without a second admin act —
accepted on the one-team trust boundary this server assumes (the same stance
as the SSRF note in §5.29), because a group whose scope stays coherent is
worth more than a review of each added file. The same repo imported by two
people is two independent groups, each moving alone; the qualified names
collide and resolve own-over-global, exactly as §5.29 says.

The UI mirrors the invariant: the visibility badge sits on a repo group's
heading rather than on each row, because the group is what moves. Who owns
which group is the Admin dialog's Skills tab, not a badge in Settings.

### 5.32 A project's environment is write-only, like every other credential here

Decided 2026-08-26. A project carries an environment — the variables its
container is created with — and it is stored the way this server stores every
other credential: **sealed at rest, masked in every response, replaceable but
never readable back**. A value is visible while it is being typed and never
again. Names stay plaintext, so one variable can be rewritten without
retyping its neighbours, and so the audit log, error messages and operational
questions ("which project sets `GITHUB_TOKEN`?") stay answerable.

**The seal is bound to the project.** A value is sealed with the encryption AAD
set to the project id, so its ciphertext opens only under that project. An
attacker with DB write access but not the key cannot paste a victim project's
ciphertext into a project they own and read the plaintext back — the box
refuses to open it there, rather than acting as a decryption oracle. This
changed the seal format: env values sealed before the upgrade do not open
afterward and must be re-entered, consistent with the project's
rebuild-on-schema-change norm.

The alternative was a per-entry flag letting the author mark which values are
secret, masking only those. It reads well — most variables are configuration,
not credentials, and `NODE_ENV` is friendlier visible. It was dropped for two
reasons. It makes this the ONE credential surface here whose visibility is a
per-item choice, against provider keys, MCP client secrets and headers, SSH
passwords and trigger secrets, which are all unconditionally write-only; a
second rule needs a reason better than convenience. And a forgotten flag
writes a token to a readable field silently, which is a failure mode with no
upper bound, while the flag's benefit has a small one.

The price is real and is paid on the ordinary values: confirming that `TZ`
says what you think needs a look inside the container. That look is one `env`
away in a terminal the workbench already offers — and it is the honest place
to look, because **the environment is readable to everything running in that
container anyway**. Sealing and masking defend the database and the screen;
nothing here hides a value from the model, and the UI says so rather than
implying a lock it does not have. Real isolation would need a credential
broker outside the container — a different feature, not a flag on this one.

The environment is a project's CONTENT, not its identity: it may be edited
while sessions are bound (workbench invariant 27 freezes which tree a session
uses, never what the tree's container is configured with), and the edit
reaches everyone at their next run through the runtime generation, exactly as
a template edit does.

### 5.33 Storage is a volume the delete destroys

Decided 2026-08-28. **The target/template split this section introduced was
reversed the same week — see §5.36 for what replaced it and why.** What
follows stands.

**One runtime axis.** The instance cache and the terminal registry used to
fence on a config generation AND a project generation, two maps that must not
reach each other's rows. So the runtime generation lives only on the PROJECT,
and a content change to a sandbox bumps it on every project that names the row
(`ProjectStore.BumpRuntimeGen`). A sandbox keeps only `revision`, the
compare-and-set every update lands against. Everything downstream —
`RetireProject`, `CloseProjectTerminals`, the `(project, gen)` cache key —
now has exactly one thing to watch. The write amplification is one UPDATE on
a rare admin edit.

**Storage is a volume, always.** The local daemon's bind mount is gone, and
with it `--workspace`, the host-path plumbing, the `DOCKER_HOST` guard that
kept file tools and containers on one filesystem, and the uid:gid default that
made bind-mounted files belong to the operator — and, because that default
existed, kept the container unable to install a package into itself. A
container now runs as the image's user (root, unless a template says
otherwise): the container is the isolation boundary, and its files live in a
volume nothing else mounts. The price is that the tree is no longer a
directory on the operator's machine; `docker cp` and an export route are how
it comes out.

**A project delete destroys its storage.** "Storage outlives the row" (§5.28)
was defensible when the tree was a visible directory the operator could find
again. A volume nobody has a listing for is an unbounded leak, and the row was
the only handle on it. Deleting a project now removes the container AND the
volume; the confirm says so. Sessions still block the delete, as before.

**No project, no sandbox tools.** The per-owner "scratch" project a run
without one used to land in is gone. It existed to make an unbound run
useful, and instead made "which tree did that command touch?" a question with
a surprising answer. An agent with no project is a chat: `attachSandboxTools`
returns early, and the composer's picker offers projects with an explicit
None.

**The session binding collapses to `project_id`.** A project pins its machine,
so the second column was derivable and could only ever disagree.

### 5.34 One E2B-compatible backend, written here, not one backend per cloud

Decided 2026-08-28. Alibaba Cloud's Function Compute cloud sandbox is **E2B
SDK compatible**, and its compatibility list covers everything the workbench
needs: create / connect / pause / kill / setTimeout, `getHost(port)`,
`commands.run`, `pty`, and the whole `files` surface. So the second backend is
not "e2b and Alibaba Cloud" — it is **one backend that speaks the E2B API, and
a target row that says which service**: `api_url` (control plane), `domain`
(the suffix a sandbox's public hosts are built from) and `api_key`. E2B's own
cloud, a self-hosted E2B and the compatible services differ only in those
three. No `flavor` discriminator: the moment one appears that configuration
cannot express, that is a new decision to argue, not a switch to grow.

**The client is written here.** E2B ships no official Go SDK — the two that
exist are community ports — and the parts this needs are six REST calls and a
handful of RPCs. The control plane is ordinary JSON over HTTP. The data plane
is Connect, whose JSON codec makes a unary call an ordinary POST and a server
stream the same JSON inside a five-byte envelope: one flag byte, four bytes of
big-endian length, the payload. That is ~150 lines, against a protobuf
toolchain plus generated stubs plus two module dependencies. The trade is real
and named: generated stubs would be checked against the schema, and this is
checked against a fake and a probe. If the surface grows past the six messages
it pins, generate them.

Because none of it needs a dependency, `sandbox/e2b` lives in the ROOT module.
The submodule rule (§5.7) exists to keep heavy dependencies out of the core;
there are none to keep out.

**What the workbench does not use**: templates are referenced by id, never
built (the service's own console or CLI builds them); no metrics, no logs, no
fork, no snapshots, no egress rules, no volumes, no code interpreter.

Three edges the probe runs settled: MakeDir's success-on-exists is matched by
the `already_exists` code alone — both verified services send it, and message
text is never sniffed. `Destroy` forgets the sandbox id only after the kill
succeeds or the service reports it gone, so a failed Destroy retries instead
of leaking billed compute. `CreateExclusive` is atomic only in its existence
check (`set -C` inside the sandbox); the content follows as a separate
upload — no argv size ceiling, and the same non-atomic-content caveat every
backend has.

**The sandbox is remembered, not searched for.** A created sandbox's id is
written to `projects.instance_ref` before the client will use it: a sandbox
nobody recorded is billed compute nobody will ever stop, so a failure to
record fails the create. Finding it again by metadata query would need a
filter syntax the compatible services do not document identically; a column
does not.

**The lease is extended on demand, never by a keepalive.** Every control
call that takes a timeout sends `max(configured TTL, the operation's own
bound)`: a tar export or a long exec asks `ensure` for that much runway and
gets a lease that outlasts it, and a terminal opens on a freshly refreshed
full lease. The extension rides the control call the operation already
forces, keeping the client free of background goroutines — and accepting the
honest limit of a lease-based TTL: an open-ended terminal idle past one full
lease can still lose its sandbox.

**Stop is pause, and Reclaim is kill.** On these services the sandbox IS the
storage, so §5.33's "a project delete destroys its storage" needs no separate
volume removal — killing the sandbox is the whole of it. It also means
`auto_pause` matters: with it off, an expired lease destroys a working tree,
which is why the template form says so where the checkbox is. So `auto_pause`
**defaults to true** — an absent field normalizes to pause-on-expiry, the safe
default for a workbench that stores working trees; an explicit `false` opts
into kill-on-expiry. The canonical stored form carries the field explicitly (no
`omitempty`), since a plain bool cannot tell an omitted key from a typed
`false`.

**Verified against both services** (2026-08-28, E2B's cloud and Alibaba Cloud
Function Compute in ap-southeast-1). The conformance suite runs against a live
service behind the `e2b_integration` build tag: **9/9 on E2B, 8/9 on FC**.
Everything the workbench needs works on both — exec, files, the atomic
create, rename/remove, the PTY terminal and the tar export — and the Connect
JSON codec, the five-byte envelopes and the base64 payloads are byte-for-byte
what this client writes.

The four things the probe settled, none of which could have been read off a
document:

- **The daemon credential is the per-sandbox token on both**, and `AuthAuto`
  is right for both: FC mints one and refuses the API key with 403; E2B mints
  one only when the create asks for it.
- **So the create always asks (`secure: true`).** Without it E2B's daemon
  takes NO credential at all — an unauthenticated request to a non-secure
  sandbox answers 200, and the sandbox id is in the public hostname of every
  port it serves. With it, 401. This is the single most important thing the
  probe found.
- **The same protobuf is rendered differently.** E2B's envd 0.7 sends
  `"type":"FILE_TYPE_DIRECTORY"` and `"size":"220"`; FC's envd 0.5 sends
  `"type":2` and `"size":220`. A `string` field for the enum fails outright on
  FC, taking every directory listing with it. Both scalars are decoded
  loosely, and the fake serves both renderings so the suite covers the split.
- **A stock template has no `/workspace`.** The working directory the
  workbench contracts for everywhere else is not one these images ship, and
  envd refuses the exec outright (`cwd '/workspace' does not exist`) rather
  than falling back to the home directory. The client makes it on the sandbox
  it created (§2.7q), so any template works, not only one built for this.
- **envd's `Remove` is idempotent** — it answers OK for a path that was never
  there — so `RemoveFile` stats first. Every other backend reports
  `fs.ErrNotExist` there, and apply_patch's rollback tells "deleted" from "was
  never there" by exactly that.

The one thing that does NOT work on FC is `Stop`: pausing is gated behind a
per-function feature (`pauseSession`), as `autoPause` is behind snapshots, and
without them the service refuses with its own words — which the client passes
through verbatim rather than replacing with a status line. That is a service
configuration, not a client defect, and the template form says so where the
checkbox is.

### 5.35 A port preview is a gateway with a grant, not a published port

Decided 2026-08-28. Losing the host bind mount (§5.33) took away the way a
person looked at what the agent built; a **port preview** gives it back:
`/preview/<grant>/…` reverse-proxies into a service listening inside the
project's sandbox.

**Not `-p 3000:3000`.** Publishing puts a member's dev server on every
interface of the host — in a multi-user workbench that is someone else's
service, reachable by anyone who can route to the machine. Host ports are also
globally unique, so two people wanting 3000 need an allocation table nobody
asked for, and on a remote daemon a published port is on the wrong machine
anyway. The gateway has none of those problems, and it is the shape both
compatible services already use (E2B's per-port host, OpenSandbox's ingress).

**A grant, because a browser tab carries no bearer token.** Every other route
authenticates with a header; opening a URL sends none. So the owner asks the
authenticated API for a grant, and gets a short-lived, unguessable, single
(project, port) token in the path. The preview route therefore lives OUTSIDE
`/api`, where the bearer middleware would refuse it, and carries its own
per-IP rate limit — a route without a bearer must not let anyone spend our
sandbox. Grants live in memory and die with the process, which is right: a
preview is a live view of a live sandbox. They are revoked when the project is
deleted.

**The grant rides the path once, then a cookie.** A previewed page's own
sub-resources — its `/asset.js`, a redirect to `/login`, an HMR socket — are
absolute paths that carry no token, so the tokenized entry point
(`/preview/<token>/`) plants the grant in a short-lived HttpOnly `preview_token`
cookie and those requests resolve through it; a typical dev server (Vite,
Webpack, an SPA) then works through the preview instead of 404ing. `Referer`
cannot stand in — the preview origin sends `Referrer-Policy: no-referrer`
(§5.37). One preview origin means ONE active grant per browser: opening a second
project's preview replaces the cookie.

**Off by default** (`preview_enabled`). It makes whatever is listening inside
the sandbox reachable by anyone who can sign in, and that is a decision an
operator takes rather than inherits.

Two things the proxy does deliberately: it strips `Authorization` and `Cookie`
(the `preview_token` included) before forwarding, so neither the workbench's own
credential nor the grant reaches somebody's dev server; and it does not apply
this app's Content-Security-Policy to a previewed page, which is not this app and
would simply break.

A docker sandbox that joins **no network** has no address at all, and the
preview says so rather than timing out. On a remote daemon the container's
address means nothing here, so the proxy dials through the same SSH transport
the docker API uses; a `tcp://` daemon exposes its API and not its container
network, and is refused with that sentence.

**Revised 2026-08-28: the port is PUBLISHED, and the project declares it.**
The first version dialed the container's address on its docker network, which
only works where that network is routable from the server — and Docker Desktop
on macOS and Windows keeps it inside a VM, so the whole feature was dead on
the most common developer machine. It also could not reach a server bound to
`127.0.0.1` inside the container even on Linux.

So a project carries `ports`, and the container publishes each to the daemon's
loopback on an ephemeral host port (spec §2.7r). The proxy dials that. It
works on every daemon the workbench supports, including Docker Desktop.

The objection that killed publishing the first time — "it must be decided at
container create, when the port nobody has typed yet is unknown" — is answered
by putting the list on the **project**, not the sandbox: a project's container
is its own, its ports are content exactly like its environment (§5.32), and a
change replaces that one container at its next run. On the shared sandbox row
the same list would apply to every project on it and rebuild all of them.

Two costs, taken deliberately over the alternative (a TCP tunnel over
`docker exec`, which needs a forwarder inside the image):

- **You declare before you preview.** An undeclared port still resolves the old
  way, so nothing is lost where the container network IS routable, and the
  attempt is bounded so it fails in seconds with the reason instead of hanging.
- **The server inside must listen on `0.0.0.0`.** Docker forwards to the
  container's interface; a `127.0.0.1` listener is invisible through a
  published port. Most dev servers bind loopback by default, so this is said
  in three places: the port field's caption, the 502 when it happens, and
  `exec_command`'s own description when the project publishes anything — the
  model is the one starting the server.

**An e2b sandbox publishes nothing, because the service already did.** Every
port answers at `<port>-<sandbox id>.<domain>`, so there is nothing to declare
and the field is not shown. **Those hosts are PUBLIC**: probed against both
services (2026-08-28), an unauthenticated GET to an application port answers
200 while envd's own port answers 401/403. `secure: true` protects the daemon,
not the workload. So on e2b the grant is a convenience, not a gate — anyone
with the sandbox id reaches the service directly, and the UI says so.

### 5.36 A sandbox is one row, and only its identity freezes

Decided 2026-08-28, reversing §5.33's split the same week it landed. The split
into `sandbox_targets` and `sandbox_templates` was justified by REUSE — one
daemon under many images, one image on many daemons — and the justification
did not survive contact with the workbench:

- For the common case there is nothing to reuse. A local docker target's whole
  config is `{}`: the row is a name and a type, so pairing it with a template
  is pure ceremony on every project create.
- The stated rule ("a target is frozen identity, a template is editable
  content") was never what the code did. Only the type and the DESTINATION —
  `host`, or `api_url|domain` — ever froze; the SSH password, the API key and
  `data_plane_auth` sat in the "frozen" table and were always editable. The
  freeze is field-level, and one table expresses it exactly as well.
- The split generated a bug class of its own: a target and a template of
  different types. That needed a type check on the project write, another on
  the health check, a filtered dropdown in two places — and it still reached a
  person's screen as `unknown sandbox target type: e2b`, because the health
  check took the first template in the list.

So: one `sandboxes` row carries where it runs and what runs on it, and a
project names one. `SandboxIdentityChanged` freezes the type and the
destination while projects live on the row; everything else — the image, the
limits, the network, the credential, the name — edits freely and reaches bound
sessions at their next run, exactly as before. For an e2b sandbox the freeze
reaches three more fields — `template_id`, `auto_pause`, `allow_internet` —
because a `/connect` resume re-attaches to the already-provisioned instance and
cannot re-apply them: accepting the edit would look saved yet silently never
take effect, so it is `409` instead. `timeout_seconds` is exempt — resume
re-sends it, so a change lands on the next refresh. Nothing about the lifecycle
changed; what changed is that the mutability line is drawn between FIELDS
instead of between TABLES, which is where it always was.

**The cost is named, not hidden.** A second image on one remote daemon repeats
that daemon's host and credential, and rotating a key touches every row that
carries it. That was accepted deliberately, against a two-dropdown project
create and two settings lists paid on every use. `Duplicate` on a sandbox row
takes the sting out — it copies everything but the identity and the
credential, which is dropped rather than carried as a mask that would resolve
to empty on the create and look like it had copied.

**A project may still change its image.** It moves between sandboxes that
share a destination — that is what "swap the template" became — and no
further: the files live at that address and do not travel
(`ErrSandboxMoveDestination`). Both rows are read inside the write's
transaction with the destination locked, so a sandbox cannot be re-addressed
between the check and the write.

### 5.37 A port preview is served on its own origin; the E2B sandbox defaults to no network

Decided 2026-08-28, hardening §5.35's port preview and §5.34's E2B backend.

**The preview runs on a separate origin.** A preview reverse-proxies whatever a
sandbox's dev server returns — an untrusted page: agent-fetched HTML, an
AI-generated page with an injection, a compromised dependency. §5.35 stripped
the workbench's `Authorization` and `Cookie` from the request INTO the dev
server, but missed the reverse direction: the workbench's bearer token lives in
`localStorage`, and a page served from the app's own origin can read it with
`localStorage.getItem`. Stripping cookies does nothing when the credential is
not a cookie. So the preview is no longer served on the app's origin at all: a
second listener (`--preview-port`, the app port + 1 by default; or
`--preview-base-url` behind a reverse proxy) serves ONLY the proxy, on an
origin that has no app, no bearer middleware, and no stored token. A grant URL
is absolute, on that origin. The app engine stops serving `/preview/`
entirely, and the isolated engine sets `Referrer-Policy: no-referrer` so the
grant in the path does not leak through a sub-resource's `Referer`. With
`Referer` denied and the token absent from an absolute-path sub-resource's URL,
those requests carry the grant in the `preview_token` cookie the entry point
plants (§5.35). The grant stays short-lived and reusable within its TTL, not
single-use: one page pulls many sub-resources, each carrying that cookie. Previews remain off
by default (`preview_enabled`), so nothing here is reachable until an admin
turns it on.

Origin isolation, not CSP, is the mechanism: a CSP on the preview would not
stop a script that is same-origin with the token. The one universal way to deny
a page another origin's storage is to put it on another origin, and for a
self-hosted binary a second port is the isolation that works for localhost, a
LAN address and a reverse proxy alike.

**The E2B sandbox joins no network by default.** The `sandbox` package promises
isolation by default, and the docker backend keeps it (`NetworkMode("none")`).
The E2B create now sends `allow_internet_access` explicitly on every create —
`false` unless the sandbox opts in — rather than omitting it and inheriting the
service's own default, which is internet ON. The two backends now read the same
way: an un-opted-in sandbox has no outbound network. NOTE: the exact field name
and its effect on the real service could not be verified from the compatible
fake; a real-service check is owed before this is relied on.

### 5.38 A workbench docker sandbox caps memory and CPU by default

Decided 2026-08-28. A docker sandbox whose config leaves `memory_mb` or `cpus`
blank (`0`) does not run uncapped: the workbench applies a default cap
(`DefaultMemoryMB` 4096 MiB, `DefaultCPUs` 2) in `sandboxes.applyImage`. Agent-
generated code runs in that container, and an uncapped one is a host-DoS
surface — a fork bomb or a runaway allocation takes the machine, and on a shared
workbench that is everyone's machine. `0` therefore means "this default", never
"unlimited"; an operator who needs more raises the cap on the sandbox.

The cap is the workbench's, not the SDK's. The `sandbox` package still enforces
only the limits its caller sets in `Options.Limits` — its isolation-by-default
promise covers network, filesystem, capabilities and the per-command timeout,
not memory and CPU, and its package doc was corrected to stop implying
otherwise. The default belongs in the layer that knows it is running untrusted
agent code for many users; a library embedded on its own has no such context to
assume.
