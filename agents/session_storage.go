package agents

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SessionStorage is the physical layer: it reads and writes entries and
// understands nothing about what they mean.
//
// Splitting it from Session is what stopped every backend from having to
// re-answer semantic questions. A store that knew "how history becomes model
// input" had to reimplement projection, compaction-awareness and settings
// resolution, and each one drifted. A store now answers only "what is
// recorded, in what order".
type SessionStorage interface {
	// Metadata describes the session.
	Metadata(ctx context.Context) (SessionMetadata, error)

	// Append records entries in order, filling in ids and timestamps for any
	// that lack them.
	Append(ctx context.Context, entries ...SessionEntry) error

	// Entry returns one entry by id, or nil when there is none.
	Entry(ctx context.Context, id string) (*SessionEntry, error)

	// Entries returns entries in append order, paginated by cursor.
	Entries(ctx context.Context, cur Cursor) ([]SessionEntry, error)

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

// SessionMetadata describes a session without reading its contents.
type SessionMetadata struct {
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

// InMemoryStorage is a goroutine-safe SessionStorage for tests and short-lived
// conversations. History is lost when the process exits.
type InMemoryStorage struct {
	id string

	mu        sync.Mutex
	entries   []SessionEntry
	seq       int64
	createdAt time.Time
	updatedAt time.Time
	hidden    bool
	title     string
}

// NewInMemoryStorage returns empty storage. The id is cosmetic — nothing
// resolves storage by it — but it shows up in metadata and errors.
func NewInMemoryStorage(id string) *InMemoryStorage {
	now := time.Now().UTC()
	return &InMemoryStorage{id: id, createdAt: now, updatedAt: now}
}

// Metadata implements SessionStorage.
func (s *InMemoryStorage) Metadata(context.Context) (SessionMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionMetadata{
		ID:         s.id,
		Title:      s.title,
		Hidden:     s.hidden,
		CreatedAt:  s.createdAt,
		UpdatedAt:  s.updatedAt,
		EntryCount: len(s.entries),
	}, nil
}

// Append implements SessionStorage.
func (s *InMemoryStorage) Append(_ context.Context, entries ...SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prepared := PrepareAppend(entries, LeafOf(s.entries), s.seq, s.entryID)
	s.entries = append(s.entries, prepared...)
	if n := len(prepared); n > 0 {
		s.seq = prepared[n-1].Seq
	}
	s.updatedAt = time.Now().UTC()
	return nil
}

func (s *InMemoryStorage) entryID(seq int64) string {
	return fmt.Sprintf("%s-e%d", s.id, seq)
}

// Entry implements SessionStorage.
func (s *InMemoryStorage) Entry(_ context.Context, id string) (*SessionEntry, error) {
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

// Entries implements SessionStorage.
func (s *InMemoryStorage) Entries(_ context.Context, cur Cursor) ([]SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PageEntries(s.entries, cur), nil
}

// Clear implements SessionStorage.
func (s *InMemoryStorage) Clear(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
	s.updatedAt = time.Now().UTC()
	return nil
}

// PopEntry implements EntryPopper.
func (s *InMemoryStorage) PopEntry(context.Context) (*SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil, nil
	}
	last := s.entries[len(s.entries)-1]
	s.entries = s.entries[:len(s.entries)-1]
	return &last, nil
}

// ReplaceEntries implements AtomicReplacer: the swap happens under one lock.
func (s *InMemoryStorage) ReplaceEntries(_ context.Context, entries ...SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
	prepared := PrepareAppend(entries, "", s.seq, s.entryID)
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
	_ SessionStorage = (*InMemoryStorage)(nil)
	_ AtomicReplacer = (*InMemoryStorage)(nil)
	_ EntryPopper    = (*InMemoryStorage)(nil)
)

// PageEntries applies a cursor to entries already in append order. Backends
// call it so every implementation pages identically — the off-by-one between
// "after this seq" and "from this seq" is exactly what each would otherwise
// get subtly wrong on its own.
func PageEntries(entries []SessionEntry, cur Cursor) []SessionEntry {
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
	return append([]SessionEntry(nil), out...)
}

// PrepareAppend fills in the fields a store owns — id, sequence number,
// creation time — and links each entry to the branch it extends.
//
// Backends call it so every store links identically. Parent linking cannot be
// done by the caller: the id of the entry before is assigned here, so only the
// store can chain a batch. Getting it wrong produces a session that reads back
// as a set of disconnected roots, which is not a failure any test of a single
// append would catch.
//
// prevLeaf is the branch tip before this batch; idFor mints an id for the
// entry at the given sequence number.
func PrepareAppend(entries []SessionEntry, prevLeaf string, nextSeq int64, idFor func(seq int64) string) []SessionEntry {
	out := make([]SessionEntry, 0, len(entries))
	parent := prevLeaf
	for _, e := range entries {
		nextSeq++
		if e.ID == "" {
			e.ID = idFor(nextSeq)
		}
		e.Seq = nextSeq
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now().UTC()
		}
		if e.Kind == "" {
			e.Kind = EntryKindItem
		}
		if e.Kind == EntryKindLeaf {
			// A leaf move is a marker, not a node: it has no parent, and it
			// moves the tip to its target rather than extending the branch.
			if p, err := e.LeafPayload(); err == nil {
				parent = p.TargetID
			}
			out = append(out, e)
			continue
		}
		if e.ParentID == "" {
			e.ParentID = parent
		}
		parent = e.ID
		out = append(out, e)
	}
	return out
}

// ReplaceStorageEntries swaps a store's whole history. It is Clear followed by Append
// unless the store can do better; a store that can swap atomically implements
// AtomicReplacer so a failure mid-rewrite cannot leave the session empty.
func ReplaceStorageEntries(ctx context.Context, s SessionStorage, entries ...SessionEntry) error {
	if r, ok := s.(AtomicReplacer); ok {
		return r.ReplaceEntries(ctx, entries...)
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	return s.Append(ctx, entries...)
}

// AtomicReplacer is an optional SessionStorage capability: replace the entire
// history in one step. Backends that can (a file rename, a DB transaction)
// should implement it, so a rewrite cannot leave the session empty when a
// failure lands between clearing and re-adding.
type AtomicReplacer interface {
	ReplaceEntries(ctx context.Context, entries ...SessionEntry) error
}

// EntryPopper is an optional SessionStorage capability: remove and return the
// most recent entry.
//
// It is not part of SessionStorage because a run never pops — history is
// append-only from the runner's side. Only an application undoing a turn needs
// it, and requiring every backend to implement it would tax the ones that
// cannot (a server-managed conversation) for a feature the run loop does not
// use.
type EntryPopper interface {
	PopEntry(ctx context.Context) (*SessionEntry, error)
}

// PopEntry removes and returns a session's most recent entry, when the store
// supports it. It reports an error for one that does not.
func (s *Session) PopEntry(ctx context.Context) (*SessionEntry, error) {
	p, ok := s.storage.(EntryPopper)
	if !ok {
		return nil, newUserError("session storage %T cannot pop entries", s.storage)
	}
	return p.PopEntry(ctx)
}
