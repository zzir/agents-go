# Sessions

A `Session` persists conversation history across runs, so multi-turn chat needs no manual item threading: prior items are prepended to the input before the run, and the new input plus everything the run generates is saved incrementally as the run proceeds.

```go
sess := agents.NewInMemorySession()

res1, _ := agents.Run(ctx, agent, "What city is the Golden Gate Bridge in?", agents.RunOptions{Session: sess, ModelProvider: p})
// "San Francisco"

res2, _ := agents.Run(ctx, agent, "What state is it in?", agents.RunOptions{Session: sess, ModelProvider: p})
// "California" — the agent saw the previous turn
```

## The Session interface

```go
type Session interface {
	// GetItems returns stored items oldest-first. limit <= 0 returns all;
	// a positive limit returns the most recent `limit` items.
	GetItems(ctx context.Context, limit int) ([]TResponseInputItem, error)
	// AddItems appends items to the history.
	AddItems(ctx context.Context, items []TResponseInputItem) error
	// PopItem removes and returns the most recent item, or nil if empty.
	PopItem(ctx context.Context) (*TResponseInputItem, error)
	// Clear removes all items.
	Clear(ctx context.Context) error
}
```

Implement it against any store (Postgres, Redis, …). Use `agents.MarshalInputItem` / `agents.UnmarshalInputItem` for encoding — they handle two openai-go serialization quirks (assistant messages and `"type"`-less easy messages) that naive JSON round-trips get wrong.

Backends that can swap the whole history in one step should also implement the optional `agents.ItemsReplacer` capability:

```go
type ItemsReplacer interface {
	ReplaceItems(ctx context.Context, items []TResponseInputItem) error // atomic swap
}
```

History rewriters (compaction, summarization) go through `agents.ReplaceSessionItems`, which uses `ReplaceItems` when available and only falls back to the non-atomic `Clear`+`AddItems` otherwise — implementing it removes the failure window where a crash between the two calls leaves the session empty. All built-in backends (`InMemorySession`, `FileSession`, SQLite/PostgreSQL) implement it.

## Choosing an implementation

The built-ins sit on a spectrum from "zero dependencies" to "full database". They all satisfy the same interface, so you can switch later:

| Implementation | Storage | Dependencies | Module | Use when |
|---|---|---|---|---|
| `InMemorySession` | memory | none | core | tests, short-lived chats |
| `memory.FileSession` | JSONL file | none | core | single process, no database wanted |
| `sessions` (SQLite) | `.db` file | bun + driver | `sessions` | one host, but you want SQL (transactions, external querying) |
| `sessions` (PostgreSQL) | server | bun + driver | `sessions` | concurrent processes, shared/production storage |
| `openai.ConversationsSession` | OpenAI server | core (`models/openai`) | core | no local store; history lives in the OpenAI Conversations API |
| `openai.CompactionSession` | wraps another Session | core (`models/openai`) | core | auto-summarize history via `responses.compact` once it grows large |

`FileSession` and the SQLite backend overlap — both persist to one local file — and the line between them is dependencies: `FileSession` is **zero-dependency** and lives in the core module, so anyone using the SDK has it without pulling a database driver. Reach for the `sessions` module's SQLite when you specifically want SQL semantics (real transactions, querying the `.db` with other tools, an easy migration path to Postgres).

## Built-in implementations

### InMemorySession

`agents.NewInMemorySession()` — goroutine-safe, process-lifetime history. Ideal for tests. Treat returned items as read-only (they share underlying pointers with the store).

### FileSession (JSONL file)

`memory.FileSession` persists history as one JSON item per line, with zero extra dependencies. It fills the "simple local persistence" niche Python covers with `SQLiteSession`, but without a database driver (for actual SQLite/Postgres, see the `sessions` module below):

```go
import "github.com/zzir/agents-go/memory"

sess, err := memory.NewFileSession("sessions", "user-123") // sessions/user-123.jsonl
// or pin an exact path:
sess, err = memory.OpenFileSession("/var/data/chats/user-123.jsonl")
```

Properties:

- Goroutine-safe within a process — including multiple `FileSession` instances opened on the same path (they share a per-path lock). Cross-process access is **not** locked.
- Appends are written in a single `write` call; rewrites (PopItem) go through an fsynced temp file + atomic rename.
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

Both store one row per item in an `agent_messages` table, encoded with `agents.MarshalInputItem`. A single `*bun.DB` can serve many session IDs (`sessions.New(db, id)`); rows are isolated by `session_id`. One schema and CRUD path serves both backends — bun smooths over the dialect differences.

### OpenAI Conversations (server-side)

`openai.ConversationsSession` stores history **server-side** under an OpenAI conversation ID — there is no local store at all. It is the Go counterpart of Python's `OpenAIConversationsSession`. The conversation is created lazily on first use unless you attach an existing one:

```go
import "github.com/zzir/agents-go/models/openai"

sess := openai.NewConversationsSession() // reads OPENAI_API_KEY; or pass option.WithAPIKey(...)
// sess.SetConversationID("conv_existing")             // resume a known conversation
// id, _ := sess.ConversationID(ctx)                   // read/create the server-side ID

agents.Run(ctx, agent, "remember my name is Ada",
	agents.RunOptions{Session: sess, ModelProvider: openai.NewProvider()})
```

`GetItems`/`AddItems`/`PopItem`/`Clear` proxy the OpenAI Conversations API. Item conversion reuses `agents.UnmarshalInputItem`, so messages and function calls/outputs round-trip; exotic server-only item types may not. `Clear` deletes the conversation, and the next use creates a fresh one. Lives in the `models/openai` package because it needs the OpenAI client.

Before persistence each item is **sanitized** for the Conversations API (mirroring Python's `_sanitize_openai_conversation_item`): stale top-level `id`s are stripped except on reasoning items and the handful of types whose create-item schema requires an id (`mcp_call`, `web_search_call`, `item_reference`, …), the SDK-only `provider_data` field is dropped, and a reasoning item that carries neither an `id` nor `encrypted_content` is omitted entirely (the server has nothing durable to reference).

### Automatic compaction

`openai.CompactionSession` **decorates** any other `Session`, calling the OpenAI `responses.compact` API to summarize history once it grows past a threshold, then replacing the stored items with the compacted result. It is the Go counterpart of Python's `OpenAIResponsesCompactionSession`.

```go
import "github.com/zzir/agents-go/models/openai"

base := agents.NewInMemorySession() // or memory.FileSession, sessions.New, …
sess, err := openai.NewCompactionSession(base, openai.CompactionOptions{
	Model:     "gpt-4.1",  // OpenAI model used for compaction (default gpt-4.1)
	Threshold: 20,         // compact when ≥20 candidate items accumulate (default 10)
	// Mode / ShouldCompact override the defaults if needed.
}, /* option.WithAPIKey(...) */)

agents.Run(ctx, agent, "…", agents.RunOptions{Session: sess, ModelProvider: openai.NewProvider()})
```

The runner attempts compaction once, after the final output is persisted (Python compacts per turn; Go persists items per turn but compacts once per run). "Candidate" items exclude user messages and existing compaction items, matching the Python heuristic. It cannot wrap a `ConversationsSession` (that manages its own server-side history) and requires an OpenAI compaction model.

Compaction is best-effort housekeeping: by the time it runs, the run's items are already saved and the final output produced, so a compaction failure is recorded on the run's `compaction` trace span instead of failing the run. The rewrite goes through `ReplaceSessionItems`, so backends implementing `ItemsReplacer` swap history atomically.

`agents.NewSlidingWindowSession(base, cfg)` is the provider-agnostic alternative: instead of `responses.compact` it summarizes older items with any `Model` you supply, keeping the most recent `WindowSize` items intact:

```go
sess := agents.NewSlidingWindowSession(base, agents.SlidingWindowConfig{
	Threshold:    20,           // compact once ≥20 items accumulate beyond the window (default 20)
	WindowSize:   10,           // keep the newest 10 items verbatim (default 10)
	SummaryModel: summaryModel, // any Model; summarization is one blocking call
	// SummaryPrompt / ShouldCompact override the defaults.
})
```

The split point is pair-aware: a `function_call` and its `function_call_output` (and a reasoning item and its successor) always land on the same side, so neither the summarization request nor the rewritten history can contain a dangling half of a pair. An empty summary aborts the pass instead of overwriting history.

The pair-safety logic is exported as `agents.SafeSplitPoint(items, split)` for custom Session implementations that rewrite history themselves: it moves a count-based split index toward 0 until both sides are self-consistent Responses sequences, returning 0 when no valid non-empty prefix exists (skip the rewrite).

## Session semantics

- History is loaded once at run start; new items are saved incrementally — the user input up front, then each turn as it completes (matching Python's per-turn `save_result_to_session`). A cancelled or failed run therefore keeps every completed turn and loses only the in-flight one, instead of losing the whole run.
- When a run pauses for [tool approval](human_in_the_loop.md), the completed part of the turn is already saved; the pending, output-less tool calls are held back (they would break replay) and saved together with their outputs once the resumed run continues. Pass the same `Session` to `ResumeRun`.
- [Handoff input filters](handoffs.md#input-filters) do not affect what is saved: the session keeps the unfiltered conversation.
- Corrections: use `PopItem` to remove the last item (e.g. let a user edit their question):

```go
last, _ := sess.PopItem(ctx) // remove the assistant answer
last, _ = sess.PopItem(ctx)  // remove the user question
res, _ := agents.Run(ctx, agent, correctedQuestion, agents.RunOptions{Session: sess, ModelProvider: p})
```

## Combining history with new input

By default a run loads the session's stored history and appends the new input to it. Two `RunOptions` knobs (both the counterparts of Python's `RunConfig` fields) adjust this:

- **`SessionInputCallback`** — a `func(history, newInput []agents.TResponseInputItem) ([]agents.TResponseInputItem, error)` that returns the exact item list sent to the model, so it can reorder, filter, or fold history rather than plain-append. Only the **genuinely new** items — those not carried over from history — are persisted back to the session, so a callback that re-emits old items does not duplicate them. Returning an error aborts the run.

  ```go
  agents.Run(ctx, agent, input, agents.RunOptions{
      Session: sess,
      SessionInputCallback: func(history, newInput []agents.TResponseInputItem) ([]agents.TResponseInputItem, error) {
          return append(summarize(history), newInput...), nil
      },
  })
  ```

- **`SessionSettings{Limit}`** — caps how many of the most recent items are loaded at run start (`0`, the default, loads the full history). An explicit `RunOptions.SessionSettings` wins; otherwise a `Session` may supply its own default by implementing the optional `SessionSettingsAware` capability:

  ```go
  type SessionSettingsAware interface {
      DefaultSessionSettings() SessionSettings // e.g. SessionSettings{Limit: 50}
  }
  ```

Both are ignored when no `Session` is set.

## Forking sessions

`ForkSession` clones an entire conversation; `ForkSessionAt` copies only the first _n_ items, creating a branch point. Both operate on the `Session` interface, so any combination of source and destination backends works (e.g. fork a SQLite session into an in-memory one):

```go
// Full clone — dst becomes an exact copy of src.
dst := agents.NewInMemorySession()
agents.ForkSession(ctx, src, dst)

// Branch at item 5 — dst gets items [0..4], the two sessions diverge from there.
branch := agents.NewInMemorySession()
agents.ForkSessionAt(ctx, src, branch, 5)
```

When the fork point is known by a server-assigned item ID rather than a positional index, use `IndexOfItemID` to resolve it:

```go
items, _ := src.GetItems(ctx, 0)
idx, ok := agents.IndexOfItemID(items, "msg_abc123")
if ok {
    agents.ForkSessionAt(ctx, src, branch, idx+1) // include the matched item
}
```

Only items the model produced carry IDs (output messages, function calls, reasoning items, etc.). User-created "easy" messages have no server-assigned ID and are never matched by `IndexOfItemID` — address those by position.

## Multiple sessions

One session = one conversation. Key sessions by conversation ID:

```go
func sessionFor(userID, threadID string) (agents.Session, error) {
	return memory.NewFileSession("sessions", userID+"-"+threadID)
}
```
