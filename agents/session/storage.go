package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Storage is the physical layer: it reads and writes entries and
// understands nothing about what they mean.
//
// Splitting it from Session is what stopped every backend from having to
// re-answer semantic questions. A store that knew "how history becomes model
// input" had to reimplement projection, compaction-awareness and settings
// resolution, and each one drifted. A store now answers only "what is
// recorded, in what order".
type Storage interface {
	// Metadata describes the session.
	Metadata(ctx context.Context) (Metadata, error)

	// Append records entries in order, filling in ids and timestamps for any
	// that lack them.
	Append(ctx context.Context, entries ...Entry) error

	// Entry returns one entry by id, or nil when there is none.
	Entry(ctx context.Context, id string) (*Entry, error)

	// Entries returns entries in append order, paginated by cursor.
	Entries(ctx context.Context, cur Cursor) ([]Entry, error)

	// Clear removes every entry.
	Clear(ctx context.Context) error
}

// Cursor paginates a read.
//
// It is a cursor rather than an offset because entries keep arriving: an offset
// shifts under a concurrent append, so page two silently skips or repeats. A
// sequence number does not move.
type Cursor struct {
	// AfterSeq returns entries with a higher sequence number. Zero starts at
	// the beginning.
	AfterSeq int64
	// Limit caps how many entries come back. Zero means no limit.
	//
	// A negative limit takes the most recent -Limit entries instead of the
	// oldest, which is how a run bounds the history it loads.
	Limit int
}

// Metadata describes a session without reading its contents.
type Metadata struct {
	ID string `json:"id"`
	// Title is a human-facing name, when the application sets one.
	Title string `json:"title,omitzero"`
	// Hidden marks a session that exists to serve another one — a background
	// task's private history — so listings can leave it out by default rather
	// than each caller maintaining its own filter.
	Hidden    bool      `json:"hidden,omitzero"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// EntryCount is how many entries the session holds, when the store can say
	// cheaply; zero otherwise.
	EntryCount int `json:"entry_count,omitzero"`
}

// InMemoryStorage is a goroutine-safe Storage for tests and short-lived
// conversations. History is lost when the process exits.
type InMemoryStorage struct {
	id string

	mu        sync.Mutex
	entries   []Entry
	seq       int64
	createdAt time.Time
	updatedAt time.Time
	hidden    bool
	title     string
	// retired marks storage whose session was deleted through the repo that
	// created it. A handle outlives the delete, and a write through one would
	// otherwise land in a map nothing reads — orphan entries by construction.
	retired bool
}

// retire marks the storage as belonging to a deleted session; every later
// write refuses with ErrNotFound.
func (s *InMemoryStorage) retire() {
	s.mu.Lock()
	s.retired = true
	s.mu.Unlock()
}

// checkLive reports whether writes are still allowed. Callers hold s.mu.
func (s *InMemoryStorage) checkLive() error {
	if s.retired {
		return fmt.Errorf("session %s: %w", s.id, ErrNotFound)
	}
	return nil
}

// NewInMemoryStorage returns empty storage. The id is cosmetic — nothing
// resolves storage by it — but it shows up in metadata and errors.
func NewInMemoryStorage(id string) *InMemoryStorage {
	now := time.Now().UTC()
	return &InMemoryStorage{id: id, createdAt: now, updatedAt: now}
}

// Metadata implements Storage.
func (s *InMemoryStorage) Metadata(context.Context) (Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Metadata{
		ID:         s.id,
		Title:      s.title,
		Hidden:     s.hidden,
		CreatedAt:  s.createdAt,
		UpdatedAt:  s.updatedAt,
		EntryCount: len(s.entries),
	}, nil
}

// Append implements Storage.
func (s *InMemoryStorage) Append(_ context.Context, entries ...Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLive(); err != nil {
		return err
	}
	prepared := PrepareAppend(entries, AppendPoint{Leaf: LeafOf(s.entries), LastSeq: s.seq})
	s.entries = append(s.entries, prepared...)
	if n := len(prepared); n > 0 {
		s.seq = prepared[n-1].Seq
	}
	s.updatedAt = time.Now().UTC()
	return nil
}

// Entry implements Storage.
func (s *InMemoryStorage) Entry(_ context.Context, id string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID == id {
			e := s.entries[i]
			return &e, nil
		}
	}
	return nil, nil
}

// Entries implements Storage.
func (s *InMemoryStorage) Entries(_ context.Context, cur Cursor) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PageEntries(s.entries, cur), nil
}

// Clear implements Storage.
func (s *InMemoryStorage) Clear(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLive(); err != nil {
		return err
	}
	s.entries = nil
	s.updatedAt = time.Now().UTC()
	return nil
}

// PopEntry implements EntryPopper.
func (s *InMemoryStorage) PopEntry(context.Context) (*Entry, error) {
	return s.pop(PopLast)
}

// PopItem implements ItemPopper.
func (s *InMemoryStorage) PopItem(context.Context) (*Entry, error) {
	return s.pop(PopLastItem)
}

func (s *InMemoryStorage) pop(mode PopMode) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLive(); err != nil {
		return nil, err
	}
	plan, ok := PlanPop(s.entries, mode)
	if !ok {
		return nil, nil
	}
	s.entries = ApplyRemoval(s.entries, plan)
	s.updatedAt = time.Now().UTC()
	return &plan.Entry, nil
}

// ReplaceEntries implements AtomicReplacer: the swap happens under one lock.
func (s *InMemoryStorage) ReplaceEntries(_ context.Context, entries ...Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLive(); err != nil {
		return err
	}
	s.entries = nil
	prepared := PrepareAppend(entries, AppendPoint{LastSeq: s.seq})
	s.entries = prepared
	if n := len(prepared); n > 0 {
		s.seq = prepared[n-1].Seq
	}
	s.updatedAt = time.Now().UTC()
	return nil
}

// SetHidden marks the session as serving another one.
func (s *InMemoryStorage) SetHidden(hidden bool) {
	s.mu.Lock()
	s.hidden = hidden
	s.mu.Unlock()
}

// SetTitle sets the session's human-facing name.
func (s *InMemoryStorage) SetTitle(title string) {
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
}

var (
	_ Storage        = (*InMemoryStorage)(nil)
	_ AtomicReplacer = (*InMemoryStorage)(nil)
	_ EntryPopper    = (*InMemoryStorage)(nil)
	_ ItemPopper     = (*InMemoryStorage)(nil)
)

// PageEntries applies a cursor to entries already in append order. Backends
// call it so every implementation pages identically — the off-by-one between
// "after this seq" and "from this seq" is exactly what each would otherwise
// get subtly wrong on its own.
func PageEntries(entries []Entry, cur Cursor) []Entry {
	out := entries
	if cur.AfterSeq > 0 {
		i := 0
		for i < len(out) && out[i].Seq <= cur.AfterSeq {
			i++
		}
		out = out[i:]
	}
	switch {
	case cur.Limit > 0 && cur.Limit < len(out):
		out = out[:cur.Limit]
	case cur.Limit < 0 && -cur.Limit < len(out):
		out = out[len(out)+cur.Limit:]
	}
	return append([]Entry(nil), out...)
}

// ReplaceEntries swaps a store's whole history. It is Clear followed by Append
// unless the store can do better; a store that can swap atomically implements
// AtomicReplacer so a failure mid-rewrite cannot leave the session empty.
func ReplaceEntries(ctx context.Context, s Storage, entries ...Entry) error {
	if r, ok := s.(AtomicReplacer); ok {
		return r.ReplaceEntries(ctx, entries...)
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	return s.Append(ctx, entries...)
}

// AtomicReplacer is an optional Storage capability: replace the entire
// history in one step. Backends that can (a file rename, a DB transaction)
// should implement it, so a rewrite cannot leave the session empty when a
// failure lands between clearing and re-adding.
type AtomicReplacer interface {
	ReplaceEntries(ctx context.Context, entries ...Entry) error
}

// EntryPopper is an optional Storage capability: remove and return the
// most recent entry.
//
// It is not part of Storage because a run never pops — history is
// append-only from the runner's side. Only an application undoing a turn needs
// it, and requiring every backend to implement it would tax the ones that
// cannot (a server-managed conversation) for a feature the run loop does not
// use.
type EntryPopper interface {
	PopEntry(ctx context.Context) (*Entry, error)
}

// ItemPopper is an optional Storage capability: remove and return the
// most recent conversation ITEM, skipping past what is not one — an error
// banner, a leaf move, an entry a compaction pass folded away.
//
// It is separate from EntryPopper because the two answer different questions,
// and only one of them is "undo the last thing that happened". A UI offering
// "undo my last message" wants this one: the banner above it is not something a
// person means to undo, and removing it would leave the turn it reports on in
// the history.
//
// Every store that can remove an entry offers both, because the choice is made
// by PlanPop rather than by each store. One interface answering both questions
// is how the same call came to mean different things in different backends.
type ItemPopper interface {
	PopItem(ctx context.Context) (*Entry, error)
}

// PopEntry removes and returns a session's most recent entry, when the store
// supports it. It reports an error for one that does not.
func (s *Session) PopEntry(ctx context.Context) (*Entry, error) {
	p, ok := s.storage.(EntryPopper)
	if !ok {
		return nil, fmt.Errorf("session: session storage %T cannot pop entries", s.storage)
	}
	return p.PopEntry(ctx)
}

// PopItem removes and returns a session's most recent conversation item,
// skipping entries that are not one. It reports an error for a store that
// cannot.
func (s *Session) PopItem(ctx context.Context) (*Entry, error) {
	p, ok := s.storage.(ItemPopper)
	if !ok {
		return nil, fmt.Errorf("session: session storage %T cannot pop items", s.storage)
	}
	return p.PopItem(ctx)
}
