# Sessions

A `Session` persists conversation history across runs, so multi-turn chat needs no manual item threading: prior items are prepended to the input before the run, and the new input plus everything the run generates is saved incrementally as the run proceeds.

```go
sess := session.NewInMemorySession()

res1, _ := agents.RunSync(ctx, agent, "What city is the Golden Gate Bridge in?", agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: p}})
// "San Francisco"

res2, _ := agents.RunSync(ctx, agent, "What state is it in?", agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: p}})
// "California" — the agent saw the previous turn
```

## Three layers

`session.Storage` reads and writes entries and knows nothing about meaning;
`session.Session` (a struct, not an interface) turns entries into what the
model reads; `session.Repo` owns lifecycles. Why it is split that way is
[Architecture](../explanation/architecture.md#sessions-are-three-layers) and
[spec §2.5c](../reference/spec.md#25c-session-layering); the methods are on
[pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents/session).

A session stores **entries**, not bare items — a Responses item plus what the
run knew about it, or something that is not a Responses item at all:

| Kind | What it holds |
|---|---|
| `item` | A Responses item — the conversation itself |
| `annotation` | For people, not the model: an error, a cancellation notice |
| `compaction` | A checkpoint: a summary standing in for the entries it folded |
| `terminal` | Interactive terminal output |
| `update` | An amendment to an earlier entry's display ([below](#entries-are-append-only)) |
| `custom` | The extension point; `CustomType` names the subtype |

The vocabulary is open: an unknown kind is ignored, not an error
([spec §2.5b](../reference/spec.md#25b-session-entries)).

## Reading: cursors, not offsets

```go
page1, _ := sess.Entries(ctx, session.Cursor{Limit: 50})
page2, _ := sess.Entries(ctx, session.Cursor{AfterSeq: page1[len(page1)-1].Seq, Limit: 50})
```

A sequence number does not move under a concurrent append; a **negative** limit
takes the most recent N. `ContextEntries` is the active branch minus what
compaction folded ([spec §2.5c](../reference/spec.md#25c-session-layering)).

## Derived state

`State` and `Stats` are folds over the entries, never fields kept beside them
([spec §2.5c](../reference/spec.md#25c-session-layering)):

```go
st, _ := sess.State(ctx)
st.LastAgent        // who spoke last
st.LastResponseID   // what server-managed conversation state chains from
st.PendingCallIDs   // tool calls recorded without outputs — a paused run
```

Two smaller reads work on items: `session.ItemText` returns one input item's
readable text (content arrives as a bare string or an array of parts — the one
place that knows both shapes) and `session.UserText` joins the user-authored
text of an input slice, e.g. for a user bubble rebuilt from pending input.

## Managing many sessions

A `session.Repo` owns which sessions exist, separately from what each one holds.

```go
repo := session.NewInMemoryRepo()                // or sessions.NewRepo(db)
sess, _ := repo.Create(ctx, session.CreateOptions{Title: "New session"})
list, _ := repo.List(ctx, session.ListOptions{})  // hidden sessions left out
```

A session exists because it was created; `Hidden` marks one that serves another
(a background task's history) and `ParentID` names which; every backend lists
newest change first, cut to `ListOptions.Limit` after the hidden filter
([spec §2.5e](../reference/spec.md#25e-session-lifecycles)). Opening a session
that does not exist is `session.ErrNotFound`, never an empty session.

## Optional storage capabilities

`AtomicReplacer` (swap the whole history in one step), `GuardedReplacer` (swap
it only while nothing was appended since the caller read — what
[automatic compaction](#automatic-compaction) uses) and `CompactionAware`
(compact its own history after a run) are optional interfaces a store may
implement; a wrapper that claims one delivers it or refuses
([spec §2.5c](../reference/spec.md#25c-session-layering)).

## Projection: what the model reads

Recording something and sending it to the model are different acts. A
`session.Projector` decides, per kind, which entries become model input:

| Kind | Projected by default? |
|---|---|
| `item` | Yes — it *is* the conversation |
| `compaction` | Structurally — its summary renders up front (as a **system** message; nobody said it) and the entries it folded are dropped |
| everything else | **No** |

Override per kind through `RunOptions.Conversation.Projectors`. The usual reason
is the opposite of suppression — letting the model see terminal output:

```go
opts.Conversation.Projectors = map[session.EntryKind]session.Projector{
	session.EntryKindTerminal: func(e session.Entry) ([]agents.InputItem, error) {
		var p struct{ Command string `json:"command"` }
		if err := json.Unmarshal(e.Payload, &p); err != nil { return nil, err }
		return agents.InputItemsFromText("$ " + p.Command), nil
	},
}
```

Mapping a kind to `nil` suppresses it instead. A runnable program is
[examples/projector](../../examples/projector/main.go).

## Entries are append-only

Nothing is ever rewritten in place
([spec §2.5b](../reference/spec.md#25b-session-entries)). A display settled
after its turn ended — a background task card — is an **update entry** naming
the one it amends, folded in by `session.FoldUpdates` at read time; an amender
that knows a tool **call id** but not the entry id names the call instead:

```go
upd, _ := session.NewUpdateEntry(targetEntryID, agents.ItemDisplay{Text: "done"})
upd, _ = session.NewCallUpdateEntry(callID, agents.ItemDisplay{Text: "done"})
sess.Append(ctx, upd)
```

An update may be stored before its target, and one whose target is missing is
ignored. Local compaction never rewrites either — it appends a
[checkpoint](#run-level-compaction); the one path that does rewrite is
`openai.CompactionSession` ([below](#automatic-compaction)).

## Choosing an implementation

The built-ins sit on a spectrum from "zero dependencies" to "full database". They are all `session.Storage` backends behind the same `session.Session` semantics layer (`InMemorySession` is the pre-wrapped convenience), so you can switch later:

| Implementation | Storage | Dependencies | Module | Use when |
|---|---|---|---|---|
| `InMemorySession` | memory | none | core | tests, short-lived chats |
| `sessions` (SQLite) | `.db` file | bun + driver | `sessions` | durable local history in one file |
| `sessions` (PostgreSQL) | server | bun + driver | `sessions` | concurrent processes, shared/production storage |
| `openai.ConversationsSession` | OpenAI server | core (`models/openai`) | core | no local store; history lives in the OpenAI Conversations API |
| `openai.CompactionSession` | wraps another Session | core (`models/openai`) | core | auto-summarize history via `responses.compact` once it grows large |

## Built-in implementations

### InMemorySession

`session.NewInMemorySession()` — goroutine-safe, process-lifetime history. Ideal for tests. Treat returned items as read-only (they share underlying pointers with the store).

### SQL sessions (SQLite / PostgreSQL)

The `github.com/zzir/agents-go/sessions` module backs a `Session` with a SQL database via [uptrace/bun](https://bun.uptrace.dev). It is a **separate Go module** so its database-driver dependencies never reach the core SDK — add it only if you use it:

```go
import "github.com/zzir/agents-go/sessions"

// SQLite — pure-Go (modernc) driver, no CGO:
sess, db, err := sessions.NewSQLite("file:/var/data/agents.db", "user-123")
defer db.Close()
err = sessions.CreateSchema(ctx, db) // once

// PostgreSQL — bring your own *sql.DB (pgx, lib/pq, or bun's pgdriver):
sess, db := sessions.NewPostgres(sqldb, "user-123")
err = sessions.CreateSchema(ctx, db)
```

Both store one row per entry in an `agent_entries` table — the whole entry serialized as JSON, with the id, parent and kind lifted into columns. A single `*bun.DB` serves many session IDs (`sessions.New(db, id)`); rows are isolated by `(session_id, gen)`, the generation a repo mints per created session, so a deleted-and-recreated id never reads its predecessor's rows.

### OpenAI Conversations (server-side)

`openai.ConversationsSession` stores history **server-side** under an OpenAI conversation ID — there is no local store at all. The conversation is created lazily on first use unless you attach an existing one:

```go
import "github.com/zzir/agents-go/models/openai"

conv := openai.NewConversationsSession() // reads OPENAI_API_KEY; or pass option.WithAPIKey(...)
// conv.SetConversationID("conv_existing")             // resume a known conversation
// id, _ := conv.ConversationID(ctx)                   // read/create the server-side ID
sess := session.NewSession(conv)                       // it is a Storage; a run takes a *session.Session

agents.Run(ctx, agent, "remember my name is Ada",
	agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: openai.NewProvider()}})
```

`Entries`/`Append`/`Clear` proxy the OpenAI Conversations API; messages and function calls/outputs round-trip, exotic server-only item types may not, and `Clear` deletes the conversation (the next use creates a fresh one). Before persistence each item is sanitized for that API: stale top-level `id`s are stripped except on reasoning items and the types whose create-item schema requires one, `provider_data` is dropped, and a reasoning item with neither an `id` nor `encrypted_content` is omitted. A runnable program is [examples/conversations](../../examples/conversations/main.go).

### Run-level compaction

Compaction is configured on the **run**, not the session — deciding what to
drop needs the model, the usage numbers and the context window, all three the
run's ([spec §2.5f](../reference/spec.md#25f-compaction)):

```go
strategy := &compaction.PipelineStrategy{Strategies: []compaction.Strategy{
	&compaction.ToolResultStrategy{Trigger: compaction.TokensExceed(60_000)},  // lossless first
	&compaction.TruncationStrategy{Trigger: compaction.TokensExceed(100_000)}, // then whole exchanges
}}
opts.Compaction = agents.CompactionOptions{Compactor: compaction.New(strategy, nil)}
```

`compaction.ContextWindowStrategy` derives both thresholds from the model's own
limits. `TruncationStrategy` drops from the oldest end but skips system groups
whatever their age; set `DropSystem: true` when the instructions are re-sent on
every run anyway.

Nothing is deleted — a strategy marks groups excluded, the log stays whole and
the model's context is a projection of it — and a compaction failure never
fails the run. `CompactionOptions.Points` selects which of the three points
(before the first model call, at each turn's save point, after the final
output) the run consults, the zero value meaning all; the save-point pass is
the one that matters for agentic work, since a run that calls thirty tools
overruns its window inside a single run. A runnable program is
[examples/runcompaction](../../examples/runcompaction/main.go).

### When the estimate is wrong

Compaction predicts. `ExecOptions.Overflow` reacts:

```go
opts.Exec.Overflow = agents.OverflowPolicy{MaxRetries: 2}
```

A model call that fails because the context did not fit triggers a compaction
pass and one more attempt at the same turn. It is off by default, the retry
does not spend the turn budget, and a pass that drops nothing buys no retry
([spec §2.5g](../reference/spec.md#25g-context-overflow)).

### Telling the model its budget

Compaction and overflow recovery act for the model. `ContextBudget` lets the
model see the figure itself:

```go
opts.Model.InputFilter = agents.ContextBudget{
	Window:   200_000, // the model's context window, in tokens
	Occupied: last,    // what the conversation's last call measured; 0 = unknown
}.InputFilter()
```

Every call then ends with one system item, `Context budget: about N of W
tokens in use (P% left).`: the run's own last call once it has one, `Occupied`
before that, nothing when neither is known. It is appended to the input, never
to the instructions, so a cached prompt prefix stays cached, and it is not
saved to the session ([spec §2.5i](../reference/spec.md#25i-the-model-manages-its-own-context)).
A runnable program is [examples/contextmanagement](../../examples/contextmanagement/main.go).

### Searching what the model no longer sees

`history.Tools` gives the model two read-only tools over its own session:
`history_search`, a case-insensitive literal substring over messages, tool
calls and tool outputs, newest first, folded history included; and
`history_read`, one item in full by id.

```go
agent.Tools = append(agent.Tools, history.Tools(history.For(sess), history.Options{})...)
```

They read the log, not the projection, so a compaction pass can fold freely:
a detail it dropped is one call away. The turn in progress is not visible
until it ends, since the session is written at turn boundaries. A storage
that leaves folded entries out of `Entries` implements
`session.HistorySearcher` to answer searches itself
([spec §2.5i](../reference/spec.md#25i-the-model-manages-its-own-context));
the workbench's SQLite and PostgreSQL store does. A `history.Resolver` lets a
host open another session for a named scope, which is how the workbench lets
a conversation search its background tasks' own transcripts. The same example
program shows the tools in use.

### Giving the model a memory

`memory.Tools` gives the model `memory_list`, `memory_read`, `memory_search`,
`memory_write` and `memory_append` over the scopes you bind. A scope is a
name the model uses, a store behind it, and your policy: writable or not,
approval or not, its limits.

```go
store := memory.NewInMemoryStore()
scopes := []memory.ScopeSpec{
	{Scope: memory.Scope{Kind: "session", ID: sessionID}, Name: "session", Writable: true,
		Describe: "this conversation's working notes; they survive compaction."},
	{Scope: memory.Scope{Kind: "agent", ID: "planner"}, Name: "agent", Writable: true, Approve: true,
		Describe: "what future conversations should know; a write waits for approval."},
}
agent.Tools = append(agent.Tools, memory.Tools(scopes, memory.Static(store))...)
```

The first scope is the default. A write to an `Approve` scope pauses the run
for approval like any approval-gated tool ([Human in the loop](human_in_the_loop.md));
a read-only scope answers a write with a refusal the model can read.
`memory.Snapshot` renders a scope as one text, which is what a context reset
carries over. A `memory.Resolver` opens the store per call for a host whose
scopes are known only then; the workbench binds the session and the agent
that way, with the rules in `store.MemoryPolicies`
([protocol](../reference/protocol.md#memories--apiv1memories)). A memory is
never sent to the model on its own: it is read through the tools, so the
context stays the log's projection
([spec §2.5i](../reference/spec.md#25i-the-model-manages-its-own-context)).

### Letting the model reset its context

`agents.NewContextTool` is `new_context`: the model asks for a fresh window,
and the run grants it at the turn's save point. A `CompactionAware` storage
compacts with `CompactionArgs.Reset` set; a run-level `compaction.Compactor`
implements `ContextResetter` and folds everything but the newest user
message, carrying `ResetSummary` (the session memory, say) into the fresh
context:

```go
compactor := compaction.New(strategy, nil)
compactor.ResetSummary = func(ctx context.Context) (string, error) {
	return memory.Snapshot(ctx, store, sessionScope, 20_000)
}
agent.Tools = append(agent.Tools, agents.NewContextTool())
opts.Compaction = agents.CompactionOptions{Compactor: compactor}
```

A session that cannot reset records `context_reset_ignored` and carries on;
a turn that ends in an interruption drops the request, and a fresh context
refuses another reset until the model has done some work
([spec §2.5i](../reference/spec.md#25i-the-model-manages-its-own-context)).
In the workbench an agent's compaction mode chooses between `summary`
(the default), `reset` and `hybrid`, and the panel's button becomes
"Reset now".

### Automatic compaction

`openai.CompactionSession` **decorates** any other `Session`, calling the OpenAI `responses.compact` API to summarize history once it grows past a threshold, then replacing the stored items with the compacted result.

```go
compacting, err := openai.NewCompactionSession(base, openai.CompactionOptions{
	Model:     "gpt-4.1", // compaction model (default gpt-4.1); Mode / ShouldCompact override the defaults
	Threshold: 20,        // compact when ≥20 candidate items accumulate (default 10)
})
sess := session.NewSession(compacting) // the decorator is a Storage; wrap it for the run
```

A runnable program is [examples/compaction](../../examples/compaction/main.go). The runner attempts compaction once, after the final output is persisted; "candidate" items exclude user messages and existing compaction items. It cannot wrap a `ConversationsSession`, requires an OpenAI compaction model, and a failure is recorded on the run's `compaction` span rather than failing the run.

This is the one path that rewrites, because the server's compact API returns a
replacement rather than a decision. The rewrite goes through
`session.GuardedReplacer`: the store swaps only while nothing has been appended
since the pass read, a pass that loses that comparison is abandoned (recorded on
the span as `abandoned`), and a backend with only `AtomicReplacer` gets the
unguarded swap ([spec §2.5f](../reference/spec.md#25f-compaction)).

Anything in the log that never reached the `previous_response_id` chain — items
after the last model response, entries a `Settings.Limit` window cut off, what
a handoff filter dropped or a projector withheld — makes the pass compact from
the stored items instead; with `Mode: CompactionModePreviousResponseID` pinned
it is skipped (`abandoned: off_chain_items`), so pinning the mode **and**
setting `Settings.Limit` means no compaction at all once the log outgrows the
window — drop one of the two
([decisions §5.51](../explanation/decisions.md#551-off-chain-history-is-decided-by-position-not-provenance)).

For the provider-agnostic, append-only alternative see [Run-level compaction](#run-level-compaction) above.

## Recovering from a crash

A process killed between issuing a tool call and recording its output leaves a
`function_call` with no output, which the Responses API refuses to load at all.
`session.Recover` repairs it:

```go
report, err := session.Recover(ctx, sess, session.RecoveryPolicy{
	RetrySafe: agents.RetrySafeNames(agent.Tools),
})
if report.NeedsRecovery() {
	log.Printf("repaired %d interrupted call(s)", len(report.Repaired))
}
```

The repair appends a synthesized error output saying the run was interrupted;
nothing is rewritten. An unfinished call is never retried by default — only a
tool with `RetrySafe: true` is left dangling for the next run to redo, and the
caller supplies that set because the stored history holds only a tool name. It
is the counterpart of [`RunState`](human_in_the_loop.md), not a replacement
([spec §2.5h](../reference/spec.md#25h-crash-recovery)).

## Session semantics

- History is loaded once at run start; new items are saved incrementally — the user input just before the first model call, then each turn as it completes — so a cancelled or failed run keeps every completed turn ([spec §2.5](../reference/spec.md#25-session-persistence-boundaries)). A save that leaves nothing behind is announced on the stream as `agents.ItemsPersistedEvent` ([Streaming](streaming.md#the-persistence-event)).
- When a run pauses for [tool approval](human_in_the_loop.md), the completed part of the turn is already saved; the pending, output-less tool calls are held back (they would break replay) and saved together with their outputs once the resumed run continues. Pass the same `Session` to `ResumeRun`.
- [Handoff input filters](handoffs.md#input-filters) do not affect what is saved: the session keeps the unfiltered conversation.
- Corrections (letting a user edit an earlier question) are a branch, not a deletion: fork the session from the entry before the question, or append the corrected turn — the projection decides what the model reads.
- What the model reads out of the loaded history is shaped by [projection](#projection-what-the-model-reads) and [compaction](#run-level-compaction), not by rewriting the load. One knob adjusts how much is loaded: `Conversation.Settings: session.Settings{Limit: 50}` caps the load to the most recent N entries; anything not positive (the zero value included) loads the full history, and it is ignored when no `Session` is set.

## Branching

A session is a **tree**, not a list
([spec §2.5d](../reference/spec.md#25d-sessions-are-trees)). Answering the same
question twice appends a second answer under the same parent, and the session
records which branch is active:

```go
// Go back to the user's message and answer it again.
sess.Branch(ctx, userEntryID)
res, _ := agents.RunSync(ctx, agent, []agents.InputItem{}, agents.RunOptions{
    Conversation: agents.ConversationOptions{Session: sess},
    Model:        agents.ModelOptions{Provider: provider},
})
```

An **empty item list** is what makes that a regeneration rather than a repeat —
`[]agents.InputItem{}`, not `""`, which appends an empty user message.

```go
leaf, _ := sess.Leaf(ctx)               // the active tip
path, _ := sess.PathEntries(ctx)        // root → leaf, the active branch only
all, _ := sess.Entries(ctx, session.Cursor{}) // everything, abandoned attempts included
```

`PathEntries` is what the model reads; `Entries` is the whole log, abandoned
attempts included, which is what a UI needs to offer the other answers.
Switching is itself an append (a leaf entry), the walk does not stop at a
compaction checkpoint, and a missing parent ends it rather than failing. A
runnable program is [examples/branching](../../examples/branching/main.go).

## Forking sessions

A fork copies history into a **separate session**; a [branch](#branching) keeps
both attempts in the *same* one. `session.Fork` clones the active branch between
any two backends — a SQLite session into an in-memory one, for instance:

```go
// Full clone — dst becomes an exact copy of src's active branch.
dst := session.NewInMemorySession()
session.Fork(ctx, src, dst)

// Point-in-time fork — compose the exported pieces: cut the branch at an
// entry, then replace the destination's contents with it.
entries, _ := src.Entries(ctx, session.Cursor{})
path := session.PathToLeaf(entries, "sess-1-e5")
session.ReplaceEntries(ctx, dst.Storage(), path...)
```

The fork point is an **entry id**, not a position: positions shift when
compaction folds entries away or a branch switch appends a leaf. Ids are
assigned by storage on append and reported by `Entries` (`e.ID`).

## Multiple sessions

One session = one conversation. Key sessions by conversation ID:

```go
func sessionFor(db *bun.DB, userID, threadID string) *session.Session {
	return sessions.New(db, userID+"-"+threadID)
}
```
