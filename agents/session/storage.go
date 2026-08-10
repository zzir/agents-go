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

// ReplaceEntries implements AtomicReplacer: the swap happens under one lock.
func (s *InMemoryStorage) ReplaceEntries(_ context.Context, entries ...Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLive(); err != nil {
		return err
	}
	s.replaceLocked(entries)
	return nil
}

// ReplaceEntriesIf implements GuardedReplacer. The comparison and the swap are
// under the lock an append takes, so nothing can land between them.
func (s *InMemoryStorage) ReplaceEntriesIf(_ context.Context, expect int64, entries ...Entry) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLive(); err != nil {
		return false, err
	}
	if AppendPointOf(s.entries).LastSeq != expect {
		return false, nil
	}
	s.replaceLocked(entries)
	return true, nil
}

// replaceLocked swaps the whole history, carrying the high-water mark over.
// Callers hold s.mu.
func (s *InMemoryStorage) replaceLocked(entries []Entry) {
	prepared := PrepareAppend(entries, AppendPoint{LastSeq: s.seq})
	s.entries = prepared
	if n := len(prepared); n > 0 {
		s.seq = prepared[n-1].Seq
	}
	s.updatedAt = time.Now().UTC()
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
	_ Storage         = (*InMemoryStorage)(nil)
	_ AtomicReplacer  = (*InMemoryStorage)(nil)
	_ GuardedReplacer = (*InMemoryStorage)(nil)
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

// GuardedReplacer is an optional Storage capability: replace the entire
// history, but only if the log has not moved since the caller read it.
//
// It exists for the one rewrite that cannot decide and write in the same step —
// a replacement computed by a server-side compaction API, with a network round
// trip in the middle. An entry appended inside that window is not in the
// replacement, and an unconditional swap deletes it: silently, and with no copy
// left anywhere.
//
// expect is the highest sequence number the store held when the caller read it,
// and zero for a log it read empty. When it still matches, the history is
// replaced and replaced is true; when it does not, nothing is written and
// replaced is false — losing the race is not an error, and what to do about it
// is the caller's decision. The comparison and the write are ONE step, taken
// under whatever the backend already serializes appends with: reading the
// current sequence number and then calling ReplaceEntries puts the window
// straight back.
//
// It catches the appends it exists for, not every possible change. A removal
// that leaves the highest sequence number in place — popping an item from under
// a newer annotation — reads as unmoved, which for the housekeeping this guards
// costs a summary that is one entry out of date.
type GuardedReplacer interface {
	ReplaceEntriesIf(ctx context.Context, expect int64, entries ...Entry) (replaced bool, err error)
}
