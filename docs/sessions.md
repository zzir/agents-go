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

## Built-in implementations

### InMemorySession

`agents.NewInMemorySession()` — goroutine-safe, process-lifetime history. Ideal for tests. Treat returned items as read-only (they share underlying pointers with the store).

### FileSession (JSONL file)

`memory.FileSession` persists history as one JSON item per line, with zero extra dependencies — the Go counterpart of Python's `SQLiteSession` niche:

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
