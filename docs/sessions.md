# Sessions

A `Session` persists conversation history across runs, so multi-turn chat needs no manual item threading: prior items are prepended to the input before the run, and the new input plus everything the run generated is saved after it completes.

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

## Choosing an implementation

The built-ins sit on a spectrum from "zero dependencies" to "full database". They all satisfy the same interface, so you can switch later:

| Implementation | Storage | Dependencies | Module | Use when |
|---|---|---|---|---|
| `InMemorySession` | memory | none | core | tests, short-lived chats |
| `memory.FileSession` | JSONL file | none | core | single process, no database wanted |
| `sessions` (SQLite) | `.db` file | bun + driver | `sessions` | one host, but you want SQL (transactions, external querying) |
| `sessions` (PostgreSQL) | server | bun + driver | `sessions` | concurrent processes, shared/production storage |
| `openai.ConversationsSession` | OpenAI server | core (`models/openai`) | core | no local store; history lives in the OpenAI Conversations API |

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
// sess = sess.WithConversationID("conv_existing")     // resume a known conversation
// id, _ := sess.ConversationID(ctx)                   // read/create the server-side ID

agents.Run(ctx, agent, "remember my name is Ada",
	agents.RunOptions{Session: sess, ModelProvider: openai.NewProvider()})
```

`GetItems`/`AddItems`/`PopItem`/`Clear` proxy the OpenAI Conversations API. Item conversion reuses `agents.UnmarshalInputItem`, so messages and function calls/outputs round-trip; exotic server-only item types may not. `Clear` deletes the conversation, and the next use creates a fresh one. Lives in the `models/openai` package because it needs the OpenAI client.

## Session semantics

- History is loaded once at run start and saved once on successful completion — a failed run saves nothing.
- When a run pauses for [tool approval](human_in_the_loop.md), nothing is saved until the resumed run completes; pass the same `Session` to `ResumeRun`.
- [Handoff input filters](handoffs.md#input-filters) do not affect what is saved: the session keeps the unfiltered conversation.
- Corrections: use `PopItem` to remove the last item (e.g. let a user edit their question):

```go
last, _ := sess.PopItem(ctx) // remove the assistant answer
last, _ = sess.PopItem(ctx)  // remove the user question
res, _ := agents.Run(ctx, agent, correctedQuestion, agents.RunOptions{Session: sess, ModelProvider: p})
```

## Multiple sessions

One session = one conversation. Key sessions by conversation ID:

```go
func sessionFor(userID, threadID string) (agents.Session, error) {
	return memory.NewFileSession("sessions", userID+"-"+threadID)
}
```
