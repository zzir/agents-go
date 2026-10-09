package session

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/tracing"
)

// Session is a conversation's history: a Storage plus what turns stored entries
// into model input. A concrete type, not an interface — spec §2.5c.
type Session struct {
	storage Storage
}

// NewSession wraps storage as a session.
func NewSession(storage Storage) *Session {
	return &Session{storage: storage}
}

// NewInMemorySession returns a session backed by in-memory storage.
func NewInMemorySession() *Session { return NewSession(NewInMemoryStorage("mem")) }

// Storage exposes the underlying store, for a capability the Session does not surface.
func (s *Session) Storage() Storage { return s.storage }

// Entries returns the session's entries in append order.
func (s *Session) Entries(ctx context.Context, cur Cursor) ([]Entry, error) {
	return s.storage.Entries(ctx, cur)
}

// ContextEntries returns the model's view: the active branch minus what
// compaction folded, checkpoints kept. cur bounds the answer, not the read;
// the whole branch is loaded — spec §2.5c.
func (s *Session) ContextEntries(ctx context.Context, cur Cursor) ([]Entry, error) {
	all, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, err
	}
	// The active branch, not append order — see ActiveBranchOf.
	path := ActiveBranchOf(all)
	if folded := FoldedEntryIDs(path); len(folded) > 0 {
		kept := make([]Entry, 0, len(path))
		for _, e := range path {
			if !folded[e.ID] {
				kept = append(kept, e)
			}
		}
		path = kept
	}
	return PageEntries(path, Cursor{AfterSeq: cur.AfterSeq, Limit: cur.Limit}), nil
}

// ContextItems returns the model input the session projects to.
func (s *Session) ContextItems(ctx context.Context, cur Cursor) ([]InputItem, error) {
	entries, err := s.ContextEntries(ctx, cur)
	if err != nil {
		return nil, err
	}
	return ProjectEntries(entries, nil)
}

// Append records entries.
func (s *Session) Append(ctx context.Context, entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.storage.Append(ctx, entries...)
}

// AppendItems records plain Responses items as item entries.
func (s *Session) AppendItems(ctx context.Context, items []InputItem, src Source) error {
	if len(items) == 0 {
		return nil
	}
	entries, err := NewItemEntries(items, src)
	if err != nil {
		return err
	}
	return s.storage.Append(ctx, entries...)
}

// Entry returns one entry by id, or nil when there is none.
func (s *Session) Entry(ctx context.Context, id string) (*Entry, error) {
	return s.storage.Entry(ctx, id)
}

// State folds the active branch into the state it implies: last agent, last
// response id, tool calls awaiting outputs — spec §2.5c.
func (s *Session) State(ctx context.Context) (DerivedState, error) {
	entries, err := s.ContextEntries(ctx, Cursor{})
	if err != nil {
		return DerivedState{}, err
	}
	return ReduceState(entries), nil
}

// Stats summarizes the session.
func (s *Session) Stats(ctx context.Context) (Stats, error) {
	entries, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return Stats{}, err
	}
	return StatsOf(entries), nil
}

// Metadata describes the session without reading its contents.
func (s *Session) Metadata(ctx context.Context) (Metadata, error) {
	return s.storage.Metadata(ctx)
}

// Clear removes every entry.
func (s *Session) Clear(ctx context.Context) error { return s.storage.Clear(ctx) }

// ErrNotFound is what a repo reports for an id it does not hold — spec §2.5e.
var ErrNotFound = errors.New("agents: session not found")

// Repo owns session lifecycles: create, open, list, delete — spec §2.5e.
type Repo interface {
	Create(ctx context.Context, opts CreateOptions) (*Session, error)
	Open(ctx context.Context, id string) (*Session, error)
	// List returns session metadata newest first, cut to ListOptions.Limit —
	// spec §2.5e2.
	List(ctx context.Context, opts ListOptions) ([]Metadata, error)
	Delete(ctx context.Context, id string) error
}

// CreateOptions configures a new session.
type CreateOptions struct {
	// ID names the session. Empty lets the repo assign one.
	ID string
	// Title is a human-facing name.
	Title string
	// Hidden marks a session that serves another (a background task's history);
	// List leaves it out by default.
	Hidden bool
	// ParentID names the session this one serves, when Hidden.
	ParentID string
}

// ListOptions filters a session listing.
type ListOptions struct {
	// IncludeHidden returns sessions that serve other sessions too.
	IncludeHidden bool
	// Limit cuts the listing from the newest end, after the hidden filter.
	// Anything not positive (the zero value included) means no limit.
	Limit int
}

// Settings configures how a run reads a Session.
type Settings struct {
	// Limit caps how many of the most recent entries a run loads at start;
	// anything not positive means no limit.
	Limit int
}

// ResolveLimit resolves how many recent entries a run loads; zero is no limit.
// A negative Settings.Limit clamps to zero: passed through, Cursor would read
// it as the oldest N.
func ResolveLimit(s Settings) int {
	if s.Limit > 0 {
		return s.Limit
	}
	return 0
}

// CompactionArgs carry what a CompactionAware storage needs to decide whether
// and how to compact after a run.
type CompactionArgs struct {
	// ResponseID is the last model response's identifier.
	ResponseID string
	// Store reports whether that response was stored server-side; nil if unknown.
	Store *bool
	// Force requests compaction regardless of the session's own decision hook.
	Force bool
	// Reset asks for a context reset rather than a summary (spec §2.5i); a
	// storage that cannot reset compacts as it would.
	Reset bool
	// StartSpan, when non-nil, opens the compaction span; call it right before
	// compacting, never on the no-op path. The runner finishes it.
	StartSpan func() *tracing.SpanHandle
}

// CompactionAware is a Storage that compacts its own history; the runner calls
// RunCompaction after a run is persisted — spec §2.5f.
type CompactionAware interface {
	RunCompaction(ctx context.Context, args CompactionArgs) error
}
