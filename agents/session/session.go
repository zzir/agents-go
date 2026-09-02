package session

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/tracing"
)

// Session is a conversation's history: a Storage plus the semantics that turn
// stored entries into what a model reads. It is a concrete type, not an
// interface — storage varies, but "how history becomes model input" does not.
type Session struct {
	storage Storage
}

// NewSession wraps storage as a session.
func NewSession(storage Storage) *Session {
	return &Session{storage: storage}
}

// NewInMemorySession returns a session backed by in-memory storage, for tests
// and short-lived conversations.
func NewInMemorySession() *Session { return NewSession(NewInMemoryStorage("mem")) }

// Storage exposes the underlying store, for callers that need a capability the
// Session does not surface.
func (s *Session) Storage() Storage { return s.storage }

// Entries returns the session's entries in append order.
func (s *Session) Entries(ctx context.Context, cur Cursor) ([]Entry, error) {
	return s.storage.Entries(ctx, cur)
}

// ContextEntries returns the entries that make up the model's view: the active
// branch with everything compaction folded away left out. The checkpoints
// themselves stay in — each carries the summary and stand-ins ProjectEntries
// renders in the folded content's place.
//
// Folding here, not only at projection, is what makes a cursor limit count
// entries the model will actually see. cur.Limit bounds the projection, not the
// storage read: the whole active branch is loaded so folding stays correct.
func (s *Session) ContextEntries(ctx context.Context, cur Cursor) ([]Entry, error) {
	all, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, err
	}
	// Walk the active branch, not append order: an abandoned attempt is still
	// recorded, and sending it would contradict the conversation. See
	// ActiveBranchOf.
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

// State folds the session's entries into the state they imply — the last agent,
// the last response id, tool calls still awaiting outputs.
//
// It folds the active branch, not append order: a dangling call on an abandoned
// attempt is not pending, since no resume can ever clear it.
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

// ErrNotFound is what a repo reports for an id it does not hold. Opening
// a session that does not exist must not look like opening an empty one: a run
// would start over instead of continuing, which is worse than an error.
var ErrNotFound = errors.New("agents: session not found")

// Repo owns session lifecycles: creating, opening, listing and deleting
// them. A backend that holds many sessions implements it once instead of every
// caller reimplementing "which sessions exist".
type Repo interface {
	Create(ctx context.Context, opts CreateOptions) (*Session, error)
	Open(ctx context.Context, id string) (*Session, error)
	// List returns session metadata ordered by UpdatedAt, newest first, cut to
	// ListOptions.Limit. Sessions sharing an UpdatedAt may come back in any order.
	// Every implementation owes the same answer here — a caller that paginated
	// correctly against one backend must not silently read the oldest sessions
	// from another (agentstest.RepoConformance checks it).
	List(ctx context.Context, opts ListOptions) ([]Metadata, error)
	Delete(ctx context.Context, id string) error
}

// CreateOptions configures a new session.
type CreateOptions struct {
	// ID names the session. Empty lets the repo assign one.
	ID string
	// Title is a human-facing name.
	Title string
	// Hidden marks a session that exists to serve another — a background task's
	// private history. List leaves hidden sessions out by default.
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
	// Limit caps how many of the most recent entries a run loads at start.
	// Anything not positive (the zero value included) means no limit — the
	// full history is loaded.
	Limit int
}

// ResolveLimit resolves how many of the most recent entries a run loads. Zero
// means no limit. A negative Settings.Limit is clamped to zero: Cursor spells
// "most recent N" as a negative limit, so passing one through would negate back
// to positive and load the oldest entries.
func ResolveLimit(s Settings) int {
	if s.Limit > 0 {
		return s.Limit
	}
	return 0
}

// CompactionArgs carry the context a CompactionAware storage needs to decide
// whether (and how) to compact its history after a run.
type CompactionArgs struct {
	// ResponseID is the last model response's identifier.
	ResponseID string
	// Store reports whether that response was stored server-side; nil if unknown.
	Store *bool
	// Force requests compaction regardless of the session's own decision hook.
	Force bool
	// OffChainItems reports that the stored history holds items the server-side
	// chain rooted at ResponseID cannot know about: what the run produced AFTER
	// that response, what a read window (Settings.Limit) left out, and what a
	// handoff input filter dropped. A storage that REPLACES the log from that
	// chain must not do so while this is set — the replacement would delete items
	// nothing ever read; compacting from the stored history is always safe. False
	// says the chain covers the whole log.
	OffChainItems bool
	// StartSpan, when non-nil, opens a compaction tracing span. Implementations
	// call it right before actually compacting (not on the no-op path, so
	// traces only show passes that did work) and may annotate the returned
	// span (e.g. before/after item counts). The runner finishes the span —
	// and records any RunCompaction error on it — after RunCompaction returns.
	StartSpan func() *tracing.SpanHandle
}

// CompactionAware is a Storage that can compact its own history — by
// summarizing older entries, or by handing them to a server-side compaction
// API. After a run is persisted, the runner calls RunCompaction so the store can
// shrink history that has grown large. It is a storage capability, not a session
// one: compaction rewrites what is stored.
type CompactionAware interface {
	RunCompaction(ctx context.Context, args CompactionArgs) error
}
