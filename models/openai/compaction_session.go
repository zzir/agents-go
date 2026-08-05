package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
)

// DefaultCompactionThreshold is the number of compaction-candidate items at
// which the default decision hook triggers compaction.
const DefaultCompactionThreshold = 10

// DefaultCompactionModel is the model used for responses.compact when none is set.
const DefaultCompactionModel = "gpt-4.1"

// CompactionMode controls how a compaction request provides the conversation.
type CompactionMode string

const (
	// CompactionModeAuto uses previous_response_id when the last response is
	// stored and has an id, otherwise the full input.
	CompactionModeAuto CompactionMode = "auto"
	// CompactionModePreviousResponseID compacts from the last response id.
	CompactionModePreviousResponseID CompactionMode = "previous_response_id"
	// CompactionModeInput compacts from the full stored input items.
	CompactionModeInput CompactionMode = "input"
)

// CompactionOptions configures a CompactionSession.
type CompactionOptions struct {
	// Model for responses.compact. Defaults to DefaultCompactionModel. Must be an
	// OpenAI model name (gpt-*, o*, or ft:gpt-*).
	Model string
	// Mode selects how history is provided to responses.compact. Defaults to
	// CompactionModeAuto.
	Mode CompactionMode
	// Threshold is the candidate-item count that triggers compaction under the
	// default decision hook. Defaults to DefaultCompactionThreshold.
	Threshold int
	// ShouldCompact overrides the default threshold check. It receives the number
	// of compaction-candidate items (non-user, non-compaction) currently stored.
	ShouldCompact func(candidateItemCount int) bool
}

// CompactionSession decorates any agents.Session, calling the OpenAI
// responses.compact API to summarize stored history once it grows past a
// threshold, then replacing the underlying history with the compacted result.
//
// The runner persists a run's items once at completion, so compaction is
// attempted once per run (when
// the run finishes and its items are saved). The decision hook still bounds how
// often the responses.compact API is actually called.
type CompactionSession struct {
	underlying agents.SessionStorage
	svc        responses.ResponseService
	model      string
	mode       CompactionMode
	should     func(int) bool
}

var (
	_ agents.SessionStorage  = (*CompactionSession)(nil)
	_ agents.CompactionAware = (*CompactionSession)(nil)
	_ agents.EntryPopper     = (*CompactionSession)(nil)
	_ agents.ItemPopper      = (*CompactionSession)(nil)
	_ agents.AtomicReplacer  = (*CompactionSession)(nil)
)

// NewCompactionSession wraps underlying with automatic responses.compact
// compaction. Pass openai-go request options (option.WithAPIKey, …) just as for
// NewProvider. It rejects wrapping a ConversationsSession (which manages its own
// server-side history) and a non-OpenAI compaction model.
func NewCompactionSession(underlying agents.SessionStorage, opts CompactionOptions, clientOpts ...option.RequestOption) (*CompactionSession, error) {
	if _, ok := underlying.(*ConversationsSession); ok {
		return nil, fmt.Errorf("CompactionSession cannot wrap a ConversationsSession (it manages its own server-side history)")
	}
	model := opts.Model
	if model == "" {
		model = DefaultCompactionModel
	}
	if !isOpenAIModelName(model) {
		return nil, fmt.Errorf("unsupported model for OpenAI responses compaction: %q", model)
	}
	mode := opts.Mode
	if mode == "" {
		mode = CompactionModeAuto
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = DefaultCompactionThreshold
	}
	should := opts.ShouldCompact
	if should == nil {
		should = func(n int) bool { return n >= threshold }
	}
	c := oai.NewClient(clientOpts...)
	return &CompactionSession{
		underlying: underlying,
		svc:        c.Responses,
		model:      model,
		mode:       mode,
		should:     should,
	}, nil
}

// Metadata implements agents.SessionStorage.
func (s *CompactionSession) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	return s.underlying.Metadata(ctx)
}

// Entries implements agents.SessionStorage.
func (s *CompactionSession) Entries(ctx context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	return s.underlying.Entries(ctx, cur)
}

// Entry implements agents.SessionStorage.
func (s *CompactionSession) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
	return s.underlying.Entry(ctx, id)
}

// Append implements agents.SessionStorage.
func (s *CompactionSession) Append(ctx context.Context, entries ...agents.SessionEntry) error {
	return s.underlying.Append(ctx, entries...)
}

// Clear implements agents.SessionStorage.
func (s *CompactionSession) Clear(ctx context.Context) error {
	return s.underlying.Clear(ctx)
}

// RunCompaction implements agents.CompactionAwareSession. It compacts the stored
// history via responses.compact when the decision hook (or args.Force) says so,
// replacing the underlying session's items with the compacted output.
func (s *CompactionSession) RunCompaction(ctx context.Context, args agents.CompactionArgs) error {
	all, err := s.underlying.Entries(ctx, agents.Cursor{})
	if err != nil {
		return err
	}
	// The replacement below is a FLAT item list — a shape that cannot express
	// a tree. A branched session (any leaf-move entry, or entries off the
	// active path) would have its abandoned attempts summarized into the
	// active context and then destroyed by the rewrite, so the pass skips
	// rather than flattens. Compaction is housekeeping: skipping costs
	// nothing but size (use run-level compaction for branching sessions).
	if isBranched(all) {
		return nil
	}
	// Server-side compaction operates on the conversation the model reads, so
	// it starts from the branch view and its projection: an annotation is not
	// context and must not be sent to responses.compact as if it were.
	entries, err := agents.NewSession(s.underlying).ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		return err
	}
	items, err := agents.ProjectEntries(entries, nil)
	if err != nil {
		return err
	}
	candidates := compactionCandidateCount(items)
	if !args.Force && !s.should(candidates) {
		return nil
	}

	// Compaction is going to happen: open the tracing span (no-op paths above
	// stay out of traces). The runner finishes it after RunCompaction returns.
	var span *tracing.SpanHandle
	if args.StartSpan != nil {
		span = args.StartSpan()
	}
	span.Set("before_items", len(items))

	mode := resolveCompactionMode(s.mode, args.ResponseID, args.Store)
	params := responses.ResponseCompactParams{Model: responses.ResponseCompactParamsModel(s.model)}
	if mode == CompactionModePreviousResponseID {
		if args.ResponseID == "" {
			return fmt.Errorf("compaction: previous_response_id mode requires a response id")
		}
		params.PreviousResponseID = oai.String(args.ResponseID)
	} else {
		params.Input = responses.ResponseCompactParamsInputUnion{OfResponseInputItemArray: items}
	}

	compacted, err := s.svc.Compact(ctx, params)
	if err != nil {
		return fmt.Errorf("compaction: %w", err)
	}

	out, err := agents.OutputToInput(compacted.Output)
	if err != nil {
		return fmt.Errorf("compaction: converting output: %w", err)
	}
	out = stripOrphanedAssistantIDs(out)
	span.Set("after_items", len(out))

	// ReplaceStorageEntries swaps atomically when the underlying session
	// supports it, so a failed write cannot leave the history cleared but empty.
	compactedEntries, err := agents.NewItemEntries(out, agents.Source{Type: agents.SourceCompaction})
	if err != nil {
		return fmt.Errorf("compaction: encoding compacted history: %w", err)
	}
	// Display records survive the rewrite. The projection above deliberately
	// excluded them from the compact INPUT — an annotation is not context —
	// and the same reasoning forbids destroying them on the way out: a
	// cancelled-run banner or a terminal record is what the projection refuses
	// to treat as history, not something history's rewrite may delete. Their
	// position among the summarized items is no longer meaningful, so they
	// carry over first, in their stored order.
	//
	// A previous compaction CHECKPOINT does not survive: its summary already
	// entered the compact input via the projection, so the output supersedes
	// it — carrying it over would front the stale summary a second time on
	// every later read, and its ExcludedIDs would name entries this rewrite
	// deletes. (Leaf moves cannot appear here — isBranched refused them — but
	// are excluded on the same grounds.)
	var replacement []agents.SessionEntry
	for _, e := range all {
		switch e.Kind {
		case agents.EntryKindItem, agents.EntryKindCompaction, agents.EntryKindLeaf:
			continue
		default:
			e.ID, e.ParentID, e.Seq = "", "", 0 // re-minted by the store, like the items'
			replacement = append(replacement, e)
		}
	}
	replacement = append(replacement, compactedEntries...)
	if err := agents.ReplaceStorageEntries(ctx, s.underlying, replacement...); err != nil {
		return fmt.Errorf("compaction: replacing history: %w", err)
	}
	return nil
}

// isBranched reports whether the session's history is a tree rather than a
// line. ActiveBranchOf is the one shared answer for what a branch view holds —
// a linkless flat history (legacy entries, custom stores) reads WHOLE there,
// so it is not "branched" merely because a raw tree walk of parentless entries
// would stop at the last one. Leaf-move entries are excluded from the walk, so
// their presence alone makes the lengths differ.
func isBranched(entries []agents.SessionEntry) bool {
	return len(agents.ActiveBranchOf(entries)) != len(entries)
}

// PopEntry implements agents.EntryPopper by delegation: wrapping a session in
// compaction must not take undo away from it — a capability is offered by
// every backend that can support it (spec §2.5e2), and the wrapper can
// whenever the wrapped store can.
func (s *CompactionSession) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	p, ok := s.underlying.(agents.EntryPopper)
	if !ok {
		return nil, fmt.Errorf("session storage %T cannot pop entries", s.underlying)
	}
	return p.PopEntry(ctx)
}

// PopItem implements agents.ItemPopper by delegation; see PopEntry.
func (s *CompactionSession) PopItem(ctx context.Context) (*agents.SessionEntry, error) {
	p, ok := s.underlying.(agents.ItemPopper)
	if !ok {
		return nil, fmt.Errorf("session storage %T cannot pop items", s.underlying)
	}
	return p.PopItem(ctx)
}

// ReplaceEntries implements agents.AtomicReplacer by delegation — and only by
// delegation: the interface PROMISES atomicity, so when the wrapped store
// cannot give it, this refuses before touching anything rather than quietly
// degrading to Clear+Append. A caller that type-asserted AtomicReplacer chose
// this method precisely to avoid the failure mode where an Append error leaves
// the history empty; handing them that failure mode anyway would make the
// assertion a lie. (RunCompaction itself still uses ReplaceStorageEntries,
// whose documented contract IS best-effort-with-fallback.)
func (s *CompactionSession) ReplaceEntries(ctx context.Context, entries ...agents.SessionEntry) error {
	r, ok := s.underlying.(agents.AtomicReplacer)
	if !ok {
		return fmt.Errorf("session storage %T cannot replace entries atomically", s.underlying)
	}
	return r.ReplaceEntries(ctx, entries...)
}

// resolveCompactionMode decides how to feed the compaction call: under "auto",
// use the full input when the last response is unstored or has no id, otherwise
// chain from previous_response_id.
func resolveCompactionMode(mode CompactionMode, responseID string, store *bool) CompactionMode {
	if mode != CompactionModeAuto {
		return mode
	}
	if store != nil && !*store {
		return CompactionModeInput
	}
	if responseID == "" {
		return CompactionModeInput
	}
	return CompactionModePreviousResponseID
}

// compactionCandidateCount counts items eligible for compaction, excluding user
// messages and existing compaction items.
func compactionCandidateCount(items []agents.TResponseInputItem) int {
	n := 0
	for _, it := range items {
		typ, role, hasContent := itemShape(it)
		isUser := (typ == "message" && role == "user") || (role == "user" && hasContent)
		if isUser || typ == "compaction" {
			continue
		}
		n++
	}
	return n
}

// stripOrphanedAssistantIDs removes the id from assistant messages when the
// compacted output carries no reasoning items, since replaying an assistant id
// without its paired reasoning item is rejected by the API.
func stripOrphanedAssistantIDs(items []agents.TResponseInputItem) []agents.TResponseInputItem {
	if len(items) == 0 {
		return items
	}
	for _, it := range items {
		if typ, _, _ := itemShape(it); typ == "reasoning" {
			return items
		}
	}
	out := make([]agents.TResponseInputItem, 0, len(items))
	for _, it := range items {
		_, role, _ := itemShape(it)
		if role == "assistant" {
			if cleaned, ok := withoutID(it); ok {
				out = append(out, cleaned)
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// itemShape extracts an input item's type, role and whether it has content, via
// a JSON round-trip (the union has too many variants to switch on directly).
func itemShape(it agents.TResponseInputItem) (typ, role string, hasContent bool) {
	b, err := agents.MarshalInputItem(it)
	if err != nil {
		return "", "", false
	}
	var probe struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(b, &probe)
	return probe.Type, probe.Role, len(probe.Content) > 0
}

// withoutID returns the item with its top-level "id" removed, or ok=false if it
// has none. It is only ever called on assistant items (OfOutputMessage); it
// makes a shallow copy so the shared session-history pointee is never mutated.
func withoutID(it agents.TResponseInputItem) (agents.TResponseInputItem, bool) {
	if it.OfOutputMessage != nil {
		msg := *it.OfOutputMessage
		msg.ID = ""
		return agents.TResponseInputItem{OfOutputMessage: &msg}, true
	}
	return it, false
}

// isOpenAIModelName reports whether a model name is a first-party OpenAI one:
// gpt-*, o<digit>*, or ft:gpt-* prefixes.
func isOpenAIModelName(model string) bool {
	m := strings.TrimSpace(model)
	if m == "" {
		return false
	}
	m = strings.TrimPrefix(m, "ft:")
	root, _, _ := strings.Cut(m, ":")
	if strings.HasPrefix(root, "gpt-") {
		return true
	}
	if len(root) >= 2 && root[0] == 'o' && root[1] >= '0' && root[1] <= '9' {
		return true
	}
	return false
}
