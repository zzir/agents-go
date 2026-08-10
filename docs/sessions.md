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

A session is split into what varies and what does not.

```go
// 1. Storage — physical reads and writes. Understands nothing about meaning.
type Storage interface {
	Metadata(ctx context.Context) (Metadata, error)
	Append(ctx context.Context, entries ...Entry) error
	Entry(ctx context.Context, id string) (*Entry, error)
	Entries(ctx context.Context, cur Cursor) ([]Entry, error)
	Clear(ctx context.Context) error
}

// 2. Session — a concrete type, not an interface. Turns entries into meaning.
sess := session.NewSession(storage)
sess.ContextItems(ctx, session.Cursor{})  // what the model reads
sess.State(ctx)                          // last agent, pending tool calls
sess.Stats(ctx)                          // counts and usage

// 3. session.Repo — lifecycles: create, open, list, delete.
```

**`Session` is a struct on purpose.** Storage varies — a file, a table, a map —
but "how history becomes model input" does not. As an interface, every backend
re-answered it, and they drifted: one store ordered compaction summaries
differently than another.

A session stores **entries**, not bare items. An entry is a Responses item plus
what the run knew about it — who produced it, how to render it, which model call
it belongs to — or something that is not a Responses item at all.

Storing items alone left everything else homeless — an error banner, the partial
output of a cancelled run, a compaction checkpoint, terminal output. Consumers
grew side tables for those and then had to merge two orderings back together at
read time.

| Kind | What it holds |
|---|---|
| `item` | A Responses item — the conversation itself |
| `annotation` | For people, not the model: an error, a cancellation notice |
| `compaction` | A checkpoint: a summary standing in for the entries it folded |
| `terminal` | Interactive terminal output |
| `update` | An amendment to an earlier entry's display (see below) |
| `custom` | The extension point; `CustomType` names the subtype |

The vocabulary is **open**: a build that meets a kind it does not know ignores
the entry rather than failing, so a session written by a newer version stays
readable.

## Reading: cursors, not offsets

```go
page1, _ := sess.Entries(ctx, session.Cursor{Limit: 50})
page2, _ := sess.Entries(ctx, session.Cursor{AfterSeq: page1[len(page1)-1].Seq, Limit: 50})
```

Entries keep arriving, so an offset shifts under a concurrent append and page
two silently skips or repeats. A sequence number does not move.

A **negative** limit takes the most recent N instead of the oldest — how a run
bounds the history it loads.

`ContextEntries` is the active branch minus what compaction folded: the
checkpoints stay in the view (they carry the summary the projection renders up
front), while the entries their exclusions name are left out — re-sending that
history would undo the compaction.

## Derived state

`State` and `Stats` are folds over the entries, not fields kept beside them.

```go
st, _ := sess.State(ctx)
st.LastAgent        // who spoke last
st.LastResponseID   // what server-managed conversation state chains from
st.PendingCallIDs   // tool calls recorded without outputs — a paused run
```

A field maintained in parallel with the log has to be updated on every write and
can disagree with the log after a crash, a concurrent writer, or a fork. A fold
cannot.

Two smaller reads work on items rather than entries: `session.ItemText` returns
one input item's readable text (content arrives as either a bare string or an
array of parts — this is the one place that knows both shapes), and
`session.UserText` joins the user-authored text of an input slice — what a user
bubble shows when rebuilding a view from a paused run's pending input.

## Managing many sessions

A `SessionRepo` owns which sessions exist, separately from what each one holds.

```go
repo, _ := filesession.NewRepo("./sessions")     // or sessions.NewRepo(db)
sess, _ := repo.Create(ctx, session.CreateOptions{Title: "New session"})
list, _ := repo.List(ctx, session.ListOptions{})  // hidden sessions left out
```

Two things it fixes:

- **A session exists because it was created**, not because it happens to have
  entries. A fresh conversation is listable before anyone speaks.
- **`Hidden` marks a session that serves another one** — a background task's
  private history. Listings exclude them by default, so every caller stops
  maintaining that filter and stops forgetting it.
- **Every backend answers a listing the same way** — newest change first, cut
  to `ListOptions.Limit` after the hidden filter, a limit that is not positive
  meaning no limit. `agentstest.RepoConformance` holds all four to it, so
  moving from the in-memory repo to SQL does not quietly change which
  conversations a sidebar shows.

**Opening a session that does not exist is an error**, never an empty one:
`session.ErrNotFound`. A typo in an id would otherwise look like a fresh
conversation, and the run would start over instead of continuing.

## Optional storage capabilities

Not every store can do everything, and the interface does not pretend otherwise.

| Capability | For |
|---|---|
| `AtomicReplacer` | Swapping the whole history in one step, so a rewrite cannot leave the session empty |
| `GuardedReplacer` | Swapping the whole history only while its highest sequence number is still the one the caller read, so a rewrite computed from a stale copy cannot delete what landed in between |
| `CompactionAware` | Compacting its own history after a run |

## Projection: what the model reads

Recording something and sending it to the model are different acts. An
`EntryProjector` decides, per kind, which entries become model input:

| Kind | Projected by default? |
|---|---|
| `item` | Yes — it *is* the conversation |
| `compaction` | Structurally — its summary renders up front (as a **system** message; nobody said it) and the entries it folded are dropped; the kept history projects from the entries themselves |
| everything else | **No** |

Override per kind through `RunOptions.Conversation.Projectors`. The usual reason
is the opposite of suppression — letting the model see terminal output:

```go
agents.RunOptions{Conversation: agents.ConversationOptions{
	Session: sess,
	Projectors: map[session.EntryKind]session.Projector{
		session.EntryKindTerminal: func(e session.Entry) ([]agents.InputItem, error) {
			var p struct{ Command string `json:"command"` }
			if err := json.Unmarshal(e.Payload, &p); err != nil { return nil, err }
			return agents.InputItemsFromText("$ " + p.Command), nil
		},
	},
}}
```

Mapping a kind to `nil` suppresses it instead.

## Entries are append-only

Nothing is ever rewritten in place. That is what lets a session be forked,
shared and read concurrently without a writer invalidating a reader's view.

Some displays are only settled long after their turn ended — a background task
card whose task runs for minutes while the parent turn finished in seconds. That
is an **update entry**: a new entry naming the one it amends, folded in by
`session.FoldUpdates` at read time.

```go
upd, _ := session.NewUpdateEntry(targetEntryID, agents.ItemDisplay{Text: "done"})
sess.Append(ctx, upd)
```

An amender that knows a tool **call id** but not the entry id — the ordinary
case for anything reporting on a tool call afterwards — names the call instead:

```go
upd, _ := session.NewCallUpdateEntry(callID, agents.ItemDisplay{Text: "done"})
sess.Append(ctx, upd)
```

Two rules make this more than a workaround:

- **An update may be stored before its target.** Association is by id, so the
  "task finished before the parent turn was persisted" race does not need
  handling — it does not exist.
- **An update whose target is missing is ignored, not an error.** The target may
  have been folded away by compaction, and failing an entire read over a stale
  pointer would make history unloadable.

Local compaction never rewrites: it appends a [checkpoint](#run-level-compaction) and the entries it folds stay exactly as they were. The one path that does rewrite is `openai.CompactionSession`, because the server-side compact API returns a replacement rather than a decision; the write is guarded instead. A backend implementing `session.GuardedReplacer` swaps the history in one step and only while its highest sequence number is still the one the pass read: a turn persisted while the compact call was in flight is not in the replacement, so writing it would delete what the pass never saw, and the pass stands down rather than doing that. A backend without the guard gets `session.ReplaceEntries`, which uses `AtomicReplacer` when it has one and only falls back to the non-atomic `Clear`+`Append` otherwise. Every built-in backend implements both, which also removes the window where a crash between the two calls leaves the session empty.

## Choosing an implementation

The built-ins sit on a spectrum from "zero dependencies" to "full database". They are all `session.Storage` backends behind the same `session.Session` semantics layer (`InMemorySession` is the pre-wrapped convenience), so you can switch later:

| Implementation | Storage | Dependencies | Module | Use when |
|---|---|---|---|---|
| `InMemorySession` | memory | none | core | tests, short-lived chats |
| `filesession.Store` | JSONL file | none | core | single process, no database wanted |
| `sessions` (SQLite) | `.db` file | bun + driver | `sessions` | one host, but you want SQL (transactions, external querying) |
| `sessions` (PostgreSQL) | server | bun + driver | `sessions` | concurrent processes, shared/production storage |
| `openai.ConversationsSession` | OpenAI server | core (`models/openai`) | core | no local store; history lives in the OpenAI Conversations API |
| `openai.CompactionSession` | wraps another Session | core (`models/openai`) | core | auto-summarize history via `responses.compact` once it grows large |

`filesession.Store` and the SQLite backend overlap — both persist to one local file — and the line between them is dependencies: `filesession.Store` is **zero-dependency** and lives in the core module, so anyone using the SDK has it without pulling a database driver. Reach for the `sessions` module's SQLite when you specifically want SQL semantics (real transactions, querying the `.db` with other tools, an easy migration path to Postgres).

## Built-in implementations

### InMemorySession

`session.NewInMemorySession()` — goroutine-safe, process-lifetime history. Ideal for tests. Treat returned items as read-only (they share underlying pointers with the store).

### filesession.Store (JSONL file)

`filesession.Store` persists history as one JSON item per line, with zero extra dependencies. It fills the "simple local persistence" niche without pulling in a database driver (for actual SQLite/Postgres, see the `sessions` module below). It is a `session.Storage`, not a `session.Session` — wrap it in `session.NewSession`:

```go
import "github.com/zzir/agents-go/filesession"

store, err := filesession.New("sessions", "user-123") // sessions/user-123.jsonl
// or pin an exact path:
store, err = filesession.NewAtPath("/var/data/chats/user-123.jsonl")

sess := session.NewSession(store) // storage alone until it is wrapped
```

Properties:

- Goroutine-safe within a process — including multiple `Store` instances opened on the same path (they share a per-path lock). Cross-process access is **not** locked.
- Appends are written in a single `write` call; rewrites (`ReplaceEntries`) go through an fsynced temp file + atomic rename.
- Corrupt lines are skipped on read rather than failing the whole session.

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

Both store one row per entry in an `agent_entries` table — the whole entry serialized as JSON, with the id, parent and kind lifted into columns for indexed lookups. A single `*bun.DB` can serve many session IDs (`sessions.New(db, id)`); rows are isolated by `session_id`. One schema and CRUD path serves both backends — bun smooths over the dialect differences.

### OpenAI Conversations (server-side)

`openai.ConversationsSession` stores history **server-side** under an OpenAI conversation ID — there is no local store at all. It is the server-side counterpart of a local session (`OpenAIConversationsSession`. The conversation is created lazily on first use unless you attach an existing one:

```go
import "github.com/zzir/agents-go/models/openai"

sess := openai.NewConversationsSession() // reads OPENAI_API_KEY; or pass option.WithAPIKey(...)
// sess.SetConversationID("conv_existing")             // resume a known conversation
// id, _ := sess.ConversationID(ctx)                   // read/create the server-side ID

agents.Run(ctx, agent, "remember my name is Ada",
	agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: openai.NewProvider()}})
```

`Entries`/`Append`/`Clear` proxy the OpenAI Conversations API. Item conversion reuses `session.UnmarshalInputItem`, so messages and function calls/outputs round-trip; exotic server-only item types may not. `Clear` deletes the conversation, and the next use creates a fresh one. Lives in the `models/openai` package because it needs the OpenAI client.

Before persistence each item is **sanitized** for the Conversations API: stale top-level `id`s are stripped except on reasoning items and the handful of types whose create-item schema requires an id (`mcp_call`, `web_search_call`, `item_reference`, …), the SDK-only `provider_data` field is dropped, and a reasoning item that carries neither an `id` nor `encrypted_content` is omitted entirely (the server has nothing durable to reference).

### Run-level compaction

Compaction is configured on the **run**, not on the session. Deciding what to
drop needs the model (to summarize), the usage numbers (to measure) and the
context window (to compare against) — all three belong to the run, and a
session decorator holding a summarization model was a shape inherited from
elsewhere.

```go
import "github.com/zzir/agents-go/agents/compaction"

strategy := &compaction.PipelineStrategy{Strategies: []compaction.Strategy{
	// Cheap and lossless first: old tool results fold to one line per tool.
	&compaction.ToolResultStrategy{Trigger: compaction.TokensExceed(60_000)},
	// Only then drop whole exchanges.
	&compaction.TruncationStrategy{Trigger: compaction.TokensExceed(100_000)},
}}

agents.Run(ctx, agent, "…", agents.RunOptions{
	Conversation: agents.ConversationOptions{Session: sess},
	Compaction:   agents.CompactionOptions{Compactor: compaction.New(strategy, nil)},
})
```

`compaction.ContextWindowStrategy` derives both thresholds from the model's own
limits, so you do not have to pick numbers that depend on the model anyway.

`TruncationStrategy` drops from the oldest end but skips system groups whatever
their age, since instructions apply to the whole conversation rather than to the
turn that carried them. Set `DropSystem: true` to drop them too — appropriate
when the instructions are re-sent on every run anyway.

**Nothing is deleted.** A strategy marks groups excluded and may leave a folded
summary behind; the stored log stays whole, and the context the model sees is a
projection of it. That is what lets a compacted session still be forked, read
concurrently, and inspected after the fact.

`Points` selects when the run consults the compactor. The zero value means all
of them:

| Point | When |
|---|---|
| `CompactBeforeRun` | after reading the session, before the first model call |
| `CompactAtSavePoint` | at each turn boundary — after the turn is persisted, before the next model call |
| `CompactAfterRun` | once the final output is persisted |

`CompactAtSavePoint` is the one that matters for agentic work: a run that calls
thirty tools overruns its window inside a single run, long before a run-level
pass would look. At that point the run rebuilds its context from the log rather
than editing the items in flight — the log is the truth, and a projection of it
cannot fall out of step with what was stored.

A compaction failure never fails the run: the context it was shrinking is still
valid, so the error is recorded on the `compaction` trace span and the run
continues with what it had.

### When the estimate is wrong

Compaction predicts. `ExecOptions.Overflow` reacts:

```go
opts.Exec.Overflow = agents.OverflowPolicy{MaxRetries: 2}
```

A model call that fails because the context did not fit triggers a compaction
pass and one more attempt at the same turn. It is off by default — an overflow
is worth reporting rather than silently shrinking the conversation — and the
retry does not spend the turn budget, since the budget counts calls the model
made and an overflow is one it never got. A pass that drops nothing buys no
retry: an identical request would fail identically.

Local compaction and server-managed history are mutually exclusive by
construction — `UsePreviousResponseID` and `ConversationID` already refuse to
combine with a local `Session`.

### Automatic compaction

`openai.CompactionSession` **decorates** any other `Session`, calling the OpenAI `responses.compact` API to summarize history once it grows past a threshold, then replacing the stored items with the compacted result.

```go
import "github.com/zzir/agents-go/models/openai"

base := session.NewInMemorySession() // or filesession.New, sessions.New, …
sess, err := openai.NewCompactionSession(base, openai.CompactionOptions{
	Model:     "gpt-4.1",  // OpenAI model used for compaction (default gpt-4.1)
	Threshold: 20,         // compact when ≥20 candidate items accumulate (default 10)
	// Mode / ShouldCompact override the defaults if needed.
}, /* option.WithAPIKey(...) */)

agents.Run(ctx, agent, "…", agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: openai.NewProvider()}})
```

The runner attempts compaction once, after the final output is persisted (items persist per turn, but compaction runs once per run). "Candidate" items exclude user messages and existing compaction items. It cannot wrap a `ConversationsSession` (that manages its own server-side history) and requires an OpenAI compaction model.

Compaction is best-effort housekeeping: by the time it runs, the run's items are already saved and the final output produced, so a compaction failure is recorded on the run's `compaction` trace span instead of failing the run. The rewrite goes through `session.GuardedReplacer`, so a backend that has it swaps history in one step and only while nothing has been appended since the pass read it; a pass that loses that comparison is abandoned, recorded on the span as `abandoned`, and the next run's pass starts from the history as it then stands. A backend with only `AtomicReplacer` gets the unguarded swap. This is the one path that still rewrites, because the server's compact API returns a replacement rather than a decision.

Four things end up in the log without ever reaching the `previous_response_id` chain: items the run produced after the last model response (a terminating tool's output, an error handler's fallback message, a steer taken past the last model call), entries a read window (`Conversation.Settings.Limit`) truncated away, whatever a handoff input filter dropped on its way to the next agent, and any entry a `Conversation.Projectors` entry withholds from the model. A pass that would replace any of them with a summary written without them compacts from the stored items instead. With `Mode: CompactionModePreviousResponseID` pinned, that pass is skipped rather than switched, and the span records `abandoned: off_chain_items` — so pinning that mode **and** setting `Settings.Limit` means no compaction at all, every run, once the log outgrows the window; drop one of the two. A log that still fits inside its window is unaffected, because the runner measures the read rather than assuming a configured window truncated it, and a projector that rewrites an entry rather than withholding it does not count.

For the provider-agnostic, append-only alternative see [Run-level compaction](#run-level-compaction) above.

## Recovering from a crash

A process killed between issuing a tool call and recording its output leaves a
`function_call` with no `function_call_output`. The Responses API rejects that
history outright, so the session is not merely untidy — it cannot be loaded at
all, and every later attempt to continue the conversation fails the same way.

```go
report, err := session.Recover(ctx, sess, session.RecoveryPolicy{
	RetrySafe: agents.RetrySafeNames(agent.Tools),
})
if report.NeedsRecovery() {
	log.Printf("repaired %d interrupted call(s)", len(report.Repaired))
}
```

The repair **appends** a synthesized error output — nothing is rewritten, so the
record of what actually happened survives. The message says the run was
interrupted and warns against assuming the tool succeeded, since a blank result
would read to the model as "the tool returned nothing".

**An unfinished call is never retried by default.** There is no way to tell
whether the tool ran: the email may already have been sent. A tool that is safe
to repeat says so, and is then left dangling for the next run to redo:

```go
readFile.RetrySafe = true
agent.Tools = []*agents.Tool{readFile, sendEmail}
```

`RetrySafe` is supplied by the caller because the stored history holds a tool
*name*, and only the caller knows the agent.

This is the counterpart of [`RunState`](human_in_the_loop.md), not a
replacement: `RunState` handles a run that paused on purpose and knows exactly
where it was, while this handles a process that died and left only what had been
written. `safePersistBoundary` already keeps a dangling call out of a session on
every ordinary exit; it cannot help when the process is killed.

## Session semantics

- History is loaded once at run start; new items are saved incrementally — the user input up front, then each turn as it completes (per-turn `save_result_to_session`). A cancelled or failed run therefore keeps every completed turn and loses only the in-flight one, instead of losing the whole run. A save that leaves nothing behind is announced on the stream as `agents.ItemsPersistedEvent`, so a consumer mirroring the run can tell buffered from persisted without inferring the SDK's timing (see [streaming](streaming.md#the-persistence-event)).
- When a run pauses for [tool approval](human_in_the_loop.md), the completed part of the turn is already saved; the pending, output-less tool calls are held back (they would break replay) and saved together with their outputs once the resumed run continues. Pass the same `Session` to `ResumeRun`.
- [Handoff input filters](handoffs.md#input-filters) do not affect what is saved: the session keeps the unfiltered conversation.
- Corrections (letting a user edit an earlier question) are a branch, not a
  deletion: fork the session from the entry before the question, or append the
  corrected turn — entries are append-only, and the projection decides what
  the model reads.

## Combining history with new input

A run loads the session's stored history and appends the new input to it. What the model reads out of that history is shaped by [projection](#projection-what-the-model-reads) and [compaction](#run-level-compaction), not by rewriting the load; one knob adjusts how much is loaded:

- **`session.Settings{Limit}`** — caps how many of the most recent entries are loaded at run start, counting from the newest end. Anything not positive (including `0`, the zero value) loads the full history: `agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess, Settings: session.Settings{Limit: 50}}}`.

It is ignored when no `Session` is set.

## Branching

A session is a **tree**, not a list. Answering the same question twice does not
overwrite the first answer: it appends a second one under the same parent, and
the session records which branch is active.

```go
// Go back to the user's message and answer it again.
sess.Branch(ctx, userEntryID)
res, _ := agents.RunSync(ctx, agent, []agents.InputItem{}, agents.RunOptions{
    Conversation: agents.ConversationOptions{Session: sess},
    Model:        agents.ModelOptions{Provider: provider},
})
```

An **empty item list** is what makes that a regeneration rather than a repeat:
there is nothing to add, and the run answers the history the branch now points
at. Note that this is `[]agents.InputItem{}` and not `""` — an empty
string is a string input, and it appends an empty user message.

Switching is itself an append — a **leaf entry** naming the new tip — so the
abandoned attempt stays in the log and switching back is just another `Branch`.
Nothing is deleted, which is what makes "show me the other answer" possible at
all.

```go
leaf, _ := sess.Leaf(ctx)               // the active tip
path, _ := sess.PathEntries(ctx)        // root → leaf, the active branch only
all, _ := sess.Entries(ctx, session.Cursor{}) // everything, abandoned attempts included
```

`PathEntries` walks parent links from the leaf and is what the model reads;
`Entries` returns the whole log, which is what a UI needs to offer the other
attempts. The difference between them is exactly the set of abandoned entries.

Two rules worth knowing:

- **The walk does not stop at a compaction checkpoint.** Folded entries are
  still on the branch — a UI shows them collapsed under the checkpoint — and
  it is the projection that keeps them out of the model's context.
- **A missing parent ends the walk rather than failing.** An ancestor may have
  been folded away; a corrupt link makes the session shorter, never unreadable.

## Forking sessions

A fork copies history into a **separate session**; a branch keeps both attempts
in the *same* one. Reach for a fork when the two conversations should be listed
and managed separately, and for [branching](#branching) when they are two
answers to the same question.

`ForkSession` clones a conversation's active branch. It takes `*Session`, so
any combination of source and destination backends works — fork a SQLite
session into an in-memory one, for instance:

```go
// Full clone — dst becomes an exact copy of src's active branch.
dst := session.NewInMemorySession()
session.Fork(ctx, src, dst)

// Point-in-time fork — compose the exported pieces: cut the branch at an
// entry, then replace the destination's contents with it.
entries, _ := src.Entries(ctx, session.Cursor{})
path := session.PathToLeaf(entries, "sess-1-e5")
session.ReplaceEntries(ctx, branch.Storage(), path...)
```

The fork point is an **entry id**, not a position. Positions shift when
compaction folds entries away or a branch switch appends a leaf; an id names
the same entry for the life of the session. Entry ids are assigned by storage
on append and reported by `Entries`:

```go
entries, _ := src.Entries(ctx, session.Cursor{})
for _, e := range entries {
    fmt.Println(e.ID, e.Kind, e.Source.Type)
}
```

## Multiple sessions

One session = one conversation. Key sessions by conversation ID:

```go
func sessionFor(userID, threadID string) (*session.Session, error) {
	storage, err := filesession.New("sessions", userID+"-"+threadID)
	if err != nil {
		return nil, err
	}
	return session.NewSession(storage), nil
}
```
