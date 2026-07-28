package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/tracing"
)

// Session is a conversation's history: a SessionStorage plus the semantics that
// turn stored entries into what a model reads.
//
// It is a concrete type, not an interface. Storage varies — a file, a table, a
// map — but "how history becomes model input" does not, and making it an
// interface meant every backend re-answered it. They drifted, which is how one
// store ended up projecting compaction summaries in a different order than
// another.
type Session struct {
	storage    SessionStorage
	projectors map[EntryKind]EntryProjector
}

// SessionOption configures a Session.
type SessionOption func(*Session)

// WithProjectors overrides how entry kinds become model input for this session.
// A run's RunOptions.Conversation.Projectors takes precedence over these.
func WithProjectors(p map[EntryKind]EntryProjector) SessionOption {
	return func(s *Session) { s.projectors = p }
}

// NewSession wraps storage as a session.
func NewSession(storage SessionStorage, opts ...SessionOption) *Session {
	s := &Session{storage: storage}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// NewInMemorySession returns a session backed by in-memory storage, for tests
// and short-lived conversations.
func NewInMemorySession() *Session { return NewSession(NewInMemoryStorage("mem")) }

// Storage exposes the underlying store, for callers that need a capability the
// Session does not surface.
func (s *Session) Storage() SessionStorage { return s.storage }

// Entries returns the session's entries in append order.
func (s *Session) Entries(ctx context.Context, cur Cursor) ([]SessionEntry, error) {
	return s.storage.Entries(ctx, cur)
}

// ContextEntries returns the entries that make up the model's view: the active
// branch with everything compaction folded away left out. The checkpoints
// themselves stay in — each carries the summary and stand-ins ProjectEntries
// renders in the folded content's place.
//
// Filtering the folded entries HERE, not just at projection, is deliberate: a
// cursor limit counts entries the model will actually see, and a Compactor fed
// this view cannot re-include what an earlier pass already folded.
func (s *Session) ContextEntries(ctx context.Context, cur Cursor) ([]SessionEntry, error) {
	all, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, err
	}
	// Walk the active branch, not the append order: an abandoned attempt is
	// still recorded, and sending it would show the model a conversation that
	// contradicts itself. A flat, linkless history reads whole — see
	// activeBranchOf.
	path := ActiveBranchOf(all)
	if folded := FoldedEntryIDs(path); len(folded) > 0 {
		kept := make([]SessionEntry, 0, len(path))
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
func (s *Session) ContextItems(ctx context.Context, cur Cursor) ([]TResponseInputItem, error) {
	entries, err := s.ContextEntries(ctx, cur)
	if err != nil {
		return nil, err
	}
	return ProjectEntries(entries, s.projectors)
}

// Append records entries.
func (s *Session) Append(ctx context.Context, entries ...SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.storage.Append(ctx, entries...)
}

// AppendItems records plain Responses items as item entries.
func (s *Session) AppendItems(ctx context.Context, items []TResponseInputItem, src Source) error {
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
func (s *Session) Entry(ctx context.Context, id string) (*SessionEntry, error) {
	return s.storage.Entry(ctx, id)
}

// State folds the session's entries into the state they imply — the last agent,
// the last response id, tool calls still awaiting outputs.
//
// It folds the ACTIVE BRANCH (the same view RecoverSession reads), not append
// order: a dangling call on an abandoned attempt is not pending — the user
// branched away from it, and no resume can ever clear it — yet folding every
// branch reported it forever, a stuck approval no action could dismiss.
func (s *Session) State(ctx context.Context) (DerivedState, error) {
	entries, err := s.ContextEntries(ctx, Cursor{})
	if err != nil {
		return DerivedState{}, err
	}
	return ReduceState(entries), nil
}

// Stats summarizes the session.
func (s *Session) Stats(ctx context.Context) (SessionStats, error) {
	entries, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return SessionStats{}, err
	}
	return Stats(entries), nil
}

// Metadata describes the session without reading its contents.
func (s *Session) Metadata(ctx context.Context) (SessionMetadata, error) {
	return s.storage.Metadata(ctx)
}

// Clear removes every entry.
func (s *Session) Clear(ctx context.Context) error { return s.storage.Clear(ctx) }

// ErrSessionNotFound is what a repo reports for an id it does not hold. Opening
// a session that does not exist must not look like opening an empty one: a run
// would start over instead of continuing, which is worse than an error.
var ErrSessionNotFound = errors.New("agents: session not found")

// SessionRepo owns session lifecycles: creating, opening, listing and deleting
// them. A backend that holds many sessions implements it once instead of every
// caller reimplementing "which sessions exist".
type SessionRepo interface {
	Create(ctx context.Context, opts CreateOptions) (*Session, error)
	Open(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context, opts ListOptions) ([]SessionMetadata, error)
	Delete(ctx context.Context, id string) error
}

// CreateOptions configures a new session.
type CreateOptions struct {
	// ID names the session. Empty lets the repo assign one.
	ID string
	// Title is a human-facing name.
	Title string
	// Hidden marks a session that exists to serve another — a background task's
	// private history. List leaves hidden sessions out by default, so callers
	// stop maintaining that filter individually and stop forgetting it.
	Hidden bool
}

// ListOptions filters a session listing.
type ListOptions struct {
	// IncludeHidden returns sessions that serve other sessions too.
	IncludeHidden bool
	// Cursor paginates the listing.
	Cursor Cursor
}

// SessionSettings configures how a run reads a Session. It is the Go counterpart
// of Python's SessionSettings.
type SessionSettings struct {
	// Limit caps how many of the most recent entries GetEntries loads at run
	// start. Zero (the default) means no limit — the full history is loaded.
	Limit int
}

// SessionInputCallback combines a session's stored history with the run's new
// input into the item list sent to the model. Returning an error aborts the run.
// It is the Go counterpart of Python's SessionInputCallback: the default (nil)
// simply appends new input to history, while a custom callback may reorder,
// filter or fold history. Only items that are genuinely new — not carried over
// from history — are persisted back to the session.
type SessionInputCallback func(history, newInput []TResponseInputItem) ([]TResponseInputItem, error)

// resolveSessionLimit resolves how many of the most recent entries a run loads.
// Zero means no limit.
func resolveSessionLimit(override *SessionSettings) int {
	if override != nil && override.Limit > 0 {
		return override.Limit
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

// CompactionAware is a SessionStorage that can compact its own history — by
// summarizing older entries, or by handing them to a server-side compaction
// API. After a run is persisted, the runner calls RunCompaction so the store can
// shrink history that has grown large.
//
// It is a storage capability, not a session one: compaction rewrites what is
// stored, and the semantics layer above has nothing to decide about it.
type CompactionAware interface {
	RunCompaction(ctx context.Context, args CompactionArgs) error
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
