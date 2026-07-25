package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/zzir/agents-go/tracing"
)

// Session persists conversation history across runs. When a Session is supplied
// via RunOptions, the runner loads prior items and prepends them to the input,
// then saves the new user input and generated items after the run completes.
//
// It is the Go counterpart of the Python SDK's Session protocol. Implementations
// live in subpackages (e.g. memory.FileSession).
type Session interface {
	// GetEntries returns stored entries oldest-first. A limit <= 0 returns all
	// entries; a positive limit returns the most recent `limit`.
	GetEntries(ctx context.Context, limit int) ([]SessionEntry, error)
	// AddEntries appends entries to the session history. An entry with no ID
	// gets one from the store; an entry with no CreatedAt gets the store's
	// clock.
	AddEntries(ctx context.Context, entries []SessionEntry) error
	// PopEntry removes and returns the most recent entry, or nil if empty.
	PopEntry(ctx context.Context) (*SessionEntry, error)
	// Clear removes all entries from the session.
	Clear(ctx context.Context) error
}

// SessionSettings configures how a run reads a Session. It is the Go counterpart
// of Python's SessionSettings.
type SessionSettings struct {
	// Limit caps how many of the most recent entries GetEntries loads at run
	// start. Zero (the default) means no limit — the full history is loaded.
	Limit int
}

// SessionSettingsAware is an optional Session capability: expose a default
// SessionSettings that a run applies unless RunOptions.Conversation.Settings overrides
// it. It mirrors Python reading session.session_settings.
type SessionSettingsAware interface {
	DefaultSessionSettings() SessionSettings
}

// SessionInputCallback combines a session's stored history with the run's new
// input into the item list sent to the model. Returning an error aborts the run.
// It is the Go counterpart of Python's SessionInputCallback: the default (nil)
// simply appends new input to history, while a custom callback may reorder,
// filter or fold history. Only items that are genuinely new — not carried over
// from history — are persisted back to the session.
type SessionInputCallback func(history, newInput []TResponseInputItem) ([]TResponseInputItem, error)

// resolveSessionLimit resolves the effective GetItems limit: an explicit
// RunOptions.SessionSettings.Limit wins, then a Session-level default, else 0
// (no limit). Mirrors Python's resolve_session_limit.
func resolveSessionLimit(override *SessionSettings, session Session) int {
	if override != nil && override.Limit > 0 {
		return override.Limit
	}
	if sa, ok := session.(SessionSettingsAware); ok {
		if d := sa.DefaultSessionSettings(); d.Limit > 0 {
			return d.Limit
		}
	}
	return 0
}

// sessionAppendedItems returns the subset of a SessionInputCallback's output
// that should be persisted to the session: the genuinely new items, excluding
// those carried over from history. It mirrors Python's content-frequency diffing
// in session_persistence.prepare_input_with_session — new input is preferred over
// history, and items matching neither (produced by the callback) are treated as
// new. Go lacks Python's object identity, so it diffs purely by serialized
// content.
func sessionAppendedItems(history, newInput, combined []TResponseInputItem) []TResponseInputItem {
	historyCounts := fingerprintCounts(history)
	newCounts := fingerprintCounts(newInput)
	var appended []TResponseInputItem
	for _, item := range combined {
		key := fingerprintInputItem(item)
		if key == "" {
			// Unfingerprintable: cannot match history, treat as new.
			appended = append(appended, item)
			continue
		}
		if newCounts[key] > 0 {
			newCounts[key]--
			appended = append(appended, item)
			continue
		}
		if historyCounts[key] > 0 {
			historyCounts[key]--
			continue
		}
		appended = append(appended, item)
	}
	return appended
}

// fingerprintInputItem returns a stable content key for an input item, or "" if
// it cannot be serialized.
func fingerprintInputItem(item TResponseInputItem) string {
	b, err := MarshalInputItem(item)
	if err != nil {
		return ""
	}
	return string(b)
}

// fingerprintCounts builds a multiset of input-item fingerprints.
func fingerprintCounts(items []TResponseInputItem) map[string]int {
	counts := make(map[string]int, len(items))
	for _, it := range items {
		if key := fingerprintInputItem(it); key != "" {
			counts[key]++
		}
	}
	return counts
}

// EntriesReplacer is an optional Session capability: atomically replace the
// entire stored history. Backends that can do this in one step (a file rename,
// a DB transaction) should implement it so a history rewrite cannot leave the
// session empty when a failure lands between clearing and re-adding.
type EntriesReplacer interface {
	ReplaceEntries(ctx context.Context, entries []SessionEntry) error
}

// ReplaceSessionEntries swaps a session's stored history. When the session
// implements EntriesReplacer the swap is atomic; otherwise it falls back to
// Clear followed by AddEntries, which can leave the session empty if AddEntries
// fails or the process crashes between the two calls.
func ReplaceSessionEntries(ctx context.Context, s Session, entries []SessionEntry) error {
	if r, ok := s.(EntriesReplacer); ok {
		return r.ReplaceEntries(ctx, entries)
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	return s.AddEntries(ctx, entries)
}

// CompactionArgs carry the context a CompactionAwareSession needs to decide
// whether (and how) to compact its history after a run.
type CompactionArgs struct {
	// ResponseID is the last model response's identifier.
	ResponseID string
	// Store reports whether that response was stored server-side; nil if unknown.
	Store *bool
	// Force requests compaction regardless of the session's own decision hook.
	Force bool
	// StartSpan, when non-nil, opens a compaction tracing span. Implementations
	// call it right before actually compacting (not on the no-op path, so
	// traces only show passes that did work) and may annotate the returned
	// span (e.g. before/after item counts). The runner finishes the span —
	// and records any RunCompaction error on it — after RunCompaction returns.
	StartSpan func() *tracing.SpanHandle
}

// CompactionAwareSession is a Session that can compact its own stored history —
// e.g. by replacing older items with a model-generated summary via the OpenAI
// responses.compact API. After a run is persisted, the runner calls
// RunCompaction so the session can shrink history when it has grown large. It is
// the Go counterpart of Python's OpenAIResponsesCompactionAwareSession.
type CompactionAwareSession interface {
	Session
	RunCompaction(ctx context.Context, args CompactionArgs) error
}

// InMemorySession is a goroutine-safe in-memory Session, useful for tests and
// short-lived conversations. History is lost when the process exits.
type InMemorySession struct {
	mu      sync.Mutex
	entries []SessionEntry
	nextID  int
}

// NewInMemorySession returns an empty in-memory session.
func NewInMemorySession() *InMemorySession { return &InMemorySession{} }

// GetEntries implements Session.
func (s *InMemorySession) GetEntries(_ context.Context, limit int) ([]SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit >= len(s.entries) {
		return append([]SessionEntry(nil), s.entries...), nil
	}
	return append([]SessionEntry(nil), s.entries[len(s.entries)-limit:]...), nil
}

// AddEntries implements Session, assigning ids and timestamps where absent.
func (s *InMemorySession) AddEntries(_ context.Context, entries []SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		s.nextID++
		s.entries = append(s.entries, stampEntry(e, fmt.Sprintf("e%d", s.nextID)))
	}
	return nil
}

// PopEntry implements Session.
func (s *InMemorySession) PopEntry(_ context.Context) (*SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil, nil
	}
	last := s.entries[len(s.entries)-1]
	s.entries = s.entries[:len(s.entries)-1]
	return &last, nil
}

// Clear implements Session.
func (s *InMemorySession) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
	return nil
}

// ReplaceEntries implements EntriesReplacer: the whole history is swapped under
// a single lock acquisition.
func (s *InMemorySession) ReplaceEntries(_ context.Context, entries []SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
	for _, e := range entries {
		s.nextID++
		s.entries = append(s.entries, stampEntry(e, fmt.Sprintf("e%d", s.nextID)))
	}
	return nil
}

var (
	_ Session         = (*InMemorySession)(nil)
	_ EntriesReplacer = (*InMemorySession)(nil)
)

// stampEntry fills in the fields a store owns: the id and the creation time. A
// caller-supplied id is kept, so an entry can be re-added (a fork, a replace)
// without losing the identity an update entry points at.
func stampEntry(e SessionEntry, id string) SessionEntry {
	if e.ID == "" {
		e.ID = id
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Kind == "" {
		e.Kind = EntryKindItem
	}
	return e
}

// MarshalEntries serializes entries to JSON, suitable for database storage.
func MarshalEntries(entries []SessionEntry) ([]byte, error) {
	if entries == nil {
		entries = []SessionEntry{}
	}
	return json.Marshal(entries)
}

// UnmarshalEntries decodes entries produced by MarshalEntries. It tolerates
// nil, empty and "null" input by returning a nil slice.
func UnmarshalEntries(data []byte) ([]SessionEntry, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var entries []SessionEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("unmarshal session entries: %w", err)
	}
	return entries, nil
}

// MarshalItems serializes a slice of input items to JSON, suitable for database
// storage. It handles nil slices gracefully (returning "[]").
func MarshalItems(items []TResponseInputItem) ([]byte, error) {
	if items == nil {
		items = []TResponseInputItem{}
	}
	return json.Marshal(items)
}

// UnmarshalItems deserializes a JSON byte slice (as produced by MarshalItems)
// back into input items. It tolerates nil, empty, and "null" inputs by returning
// a nil slice. Each element is decoded through UnmarshalInputItem so the union
// fix-ups apply: assistant messages keep their output_text/refusal content (a
// plain slice decode matches EasyInputMessageParam first and silently drops it),
// and "easy" role messages without a "type" discriminator still decode instead
// of failing. This keeps a MarshalItems→UnmarshalItems round-trip — the encoding
// external Session backends use for DB storage — lossless.
func UnmarshalItems(data []byte) ([]TResponseInputItem, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("unmarshal session items: %w", err)
	}
	items := make([]TResponseInputItem, 0, len(raws))
	for i, raw := range raws {
		item, err := UnmarshalInputItem(raw)
		if err != nil {
			return nil, fmt.Errorf("unmarshal session item %d: %w", i, err)
		}
		items = append(items, item)
	}
	return items, nil
}

// SessionItems reads a session's history as the model input items it projects
// to — the conversation as the model would see it, with annotations and other
// non-context entries left out.
//
// It is the read shortcut for callers that only care about the conversation;
// use GetEntries directly to see everything a session holds.
func SessionItems(ctx context.Context, s Session, limit int) ([]TResponseInputItem, error) {
	entries, err := s.GetEntries(ctx, limit)
	if err != nil {
		return nil, err
	}
	return ProjectEntries(entries, nil)
}

// AddSessionItems appends plain Responses items to a session as item entries.
// It is the write shortcut for callers that have items rather than entries.
func AddSessionItems(ctx context.Context, s Session, items []TResponseInputItem, src Source) error {
	if len(items) == 0 {
		return nil
	}
	entries, err := NewItemEntries(items, src)
	if err != nil {
		return err
	}
	return s.AddEntries(ctx, entries)
}
