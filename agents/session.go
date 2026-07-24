package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/zzir/agents-go/tracing"
)

// Session persists conversation history across runs. When a Session is supplied
// via RunOptions, the runner loads prior items and prepends them to the input,
// then saves the new user input and generated items after the run completes.
//
// It is the Go counterpart of the Python SDK's Session protocol. Implementations
// live in subpackages (e.g. memory.FileSession).
type Session interface {
	// GetItems returns stored items oldest-first. A limit <= 0 returns all
	// items; a positive limit returns the most recent `limit` items.
	GetItems(ctx context.Context, limit int) ([]TResponseInputItem, error)
	// AddItems appends items to the session history.
	AddItems(ctx context.Context, items []TResponseInputItem) error
	// PopItem removes and returns the most recent item, or nil if empty.
	PopItem(ctx context.Context) (*TResponseInputItem, error)
	// Clear removes all items from the session.
	Clear(ctx context.Context) error
}

// SessionSettings configures how a run reads a Session. It is the Go counterpart
// of Python's SessionSettings.
type SessionSettings struct {
	// Limit caps how many of the most recent items GetItems loads at run start.
	// Zero (the default) means no limit — the full history is loaded.
	Limit int
}

// SessionSettingsAware is an optional Session capability: expose a default
// SessionSettings that a run applies unless RunOptions.SessionSettings overrides
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

// ItemsReplacer is an optional Session capability: atomically replace the
// entire stored history with a new item list. Backends that can do this in one
// step (a file rename, a DB transaction) should implement it so history
// rewrites — compaction, summarization — cannot leave the session empty when
// a failure lands between clearing and re-adding.
type ItemsReplacer interface {
	ReplaceItems(ctx context.Context, items []TResponseInputItem) error
}

// ReplaceSessionItems swaps a session's stored history for items. When the
// session implements ItemsReplacer the swap is atomic; otherwise it falls back
// to Clear followed by AddItems, which can leave the session empty if AddItems
// fails or the process crashes between the two calls.
func ReplaceSessionItems(ctx context.Context, s Session, items []TResponseInputItem) error {
	if r, ok := s.(ItemsReplacer); ok {
		return r.ReplaceItems(ctx, items)
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	return s.AddItems(ctx, items)
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
//
// GetItems returns a copy of the item slice, but the items share underlying
// pointers with the store; treat returned items as read-only.
type InMemorySession struct {
	mu    sync.Mutex
	items []TResponseInputItem
}

// NewInMemorySession returns an empty in-memory session.
func NewInMemorySession() *InMemorySession { return &InMemorySession{} }

// GetItems implements Session.
func (s *InMemorySession) GetItems(_ context.Context, limit int) ([]TResponseInputItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit >= len(s.items) {
		return append([]TResponseInputItem(nil), s.items...), nil
	}
	return append([]TResponseInputItem(nil), s.items[len(s.items)-limit:]...), nil
}

// AddItems implements Session.
func (s *InMemorySession) AddItems(_ context.Context, items []TResponseInputItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, items...)
	return nil
}

// PopItem implements Session.
func (s *InMemorySession) PopItem(_ context.Context) (*TResponseInputItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return nil, nil
	}
	last := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	return &last, nil
}

// Clear implements Session.
func (s *InMemorySession) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = nil
	return nil
}

// ReplaceItems implements ItemsReplacer: the whole history is swapped under a
// single lock acquisition.
func (s *InMemorySession) ReplaceItems(_ context.Context, items []TResponseInputItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]TResponseInputItem(nil), items...)
	return nil
}

var (
	_ Session       = (*InMemorySession)(nil)
	_ ItemsReplacer = (*InMemorySession)(nil)
)

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
