package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Storage reads and writes entries and understands nothing about what they
// mean; the semantics live in Session (spec §2.5c).
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

// Cursor paginates a read. It pages on sequence numbers, not offsets, so a
// concurrent append cannot make a page silently skip or repeat.
type Cursor struct {
	// AfterSeq returns entries with a higher sequence number. Zero starts at
	// the beginning.
	AfterSeq int64
	// Limit caps how many entries come back. Zero means no limit; a negative
	// limit takes the most recent -Limit entries instead of the oldest.
	Limit int
}

// Metadata describes a session without reading its contents.
type Metadata struct {
	ID string `json:"id"`
	// Title is a human-facing name, when the application sets one.
	Title string `json:"title,omitzero"`
	// Hidden marks a session that exists to serve another one — a background
	// task's private history — so listings can leave it out by default.
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
	// retired marks storage whose session was deleted; a write through a handle
	// that outlives the delete would otherwise orphan entries.
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
// call it so every implementation pages identically.
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
// history only while the log has not moved since the caller read it. expect
// is the highest sequence number the store HELD at that read, zero for a log
// read empty. On a match the history is replaced and replaced is true; else
// nothing is written and replaced is false — a lost race, not an error. It
// catches appends only: a removal that keeps the highest seq reads as unmoved.
type GuardedReplacer interface {
	ReplaceEntriesIf(ctx context.Context, expect int64, entries ...Entry) (replaced bool, err error)
}
