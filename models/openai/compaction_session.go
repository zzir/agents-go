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
	"github.com/zzir/agents-go/agents/session"
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

// CompactionSession decorates any session.Storage, calling the OpenAI
// responses.compact API to summarize stored history once it grows past a
// threshold, then replacing the underlying history with the compacted result.
// Compaction is attempted once per run, when its items are saved; the decision
// hook bounds how often the API is actually called.
type CompactionSession struct {
	underlying session.Storage
	svc        responses.ResponseService
	model      string
	mode       CompactionMode
	should     func(int) bool
}

var (
	_ session.Storage         = (*CompactionSession)(nil)
	_ session.CompactionAware = (*CompactionSession)(nil)
	_ session.AtomicReplacer  = (*CompactionSession)(nil)
	_ session.GuardedReplacer = (*CompactionSession)(nil)
)

// NewCompactionSession wraps underlying with automatic responses.compact
// compaction. Pass openai-go request options (option.WithAPIKey, …) just as for
// NewProvider. It rejects wrapping a ConversationsSession (which manages its own
// server-side history) and a non-OpenAI compaction model.
func NewCompactionSession(underlying session.Storage, opts CompactionOptions, clientOpts ...option.RequestOption) (*CompactionSession, error) {
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

// Metadata implements session.Storage.
func (s *CompactionSession) Metadata(ctx context.Context) (session.Metadata, error) {
	return s.underlying.Metadata(ctx)
}

// Entries implements session.Storage.
func (s *CompactionSession) Entries(ctx context.Context, cur session.Cursor) ([]session.Entry, error) {
	return s.underlying.Entries(ctx, cur)
}

// Entry implements session.Storage.
func (s *CompactionSession) Entry(ctx context.Context, id string) (*session.Entry, error) {
	return s.underlying.Entry(ctx, id)
}

// Append implements session.Storage.
func (s *CompactionSession) Append(ctx context.Context, entries ...session.Entry) error {
	return s.underlying.Append(ctx, entries...)
}

// Clear implements session.Storage.
func (s *CompactionSession) Clear(ctx context.Context) error {
	return s.underlying.Clear(ctx)
}

// RunCompaction implements session.CompactionAware: when the decision hook (or
// args.Force) says so, it replaces the stored history with responses.compact's
// output. A nil error does not promise a replacement — the pass is abandoned
// (recorded on the span) when something was appended mid-call or the pinned
// previous_response_id never saw part of the log; compare entries to know.
func (s *CompactionSession) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	all, err := s.underlying.Entries(ctx, session.Cursor{})
	if err != nil {
		return err
	}
	// Where the log stands now; the guarded swap at the end compares it back.
	expect := session.AppendPointOf(all).LastSeq
	// The replacement is a FLAT item list, which cannot express a tree: a
	// branched session skips the pass rather than flatten abandoned attempts.
	if isBranched(all) {
		return nil
	}
	// Server-side compaction operates on the conversation the model reads:
	// the branch view and its projection, never an annotation.
	entries, err := session.NewSession(s.underlying).ContextEntries(ctx, session.Cursor{})
	if err != nil {
		return err
	}
	items, err := session.ProjectEntries(entries, nil)
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

	mode, ok := resolveCompactionMode(s.mode, args)
	if !ok {
		// A pinned previous_response_id over a log its chain never saw: skip
		// rather than summarize without those items or switch modes unasked.
		span.Set("abandoned", "off_chain_items")
		return nil
	}
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

	compactedEntries, err := session.NewItemEntries(out, agents.Source{Type: agents.SourceCompaction})
	if err != nil {
		return fmt.Errorf("compaction: encoding compacted history: %w", err)
	}
	// Display records survive the rewrite (an annotation is not context) and go
	// first; a previous compaction CHECKPOINT does not, its summary being in the input.
	var replacement []session.Entry
	for _, e := range all {
		switch e.Kind {
		case session.EntryKindItem, session.EntryKindCompaction, session.EntryKindLeaf:
			continue
		default:
			// The id is kept — an update entry names its target by it — while
			// the link is re-derived: parent and seq are the store's to assign.
			e.ParentID, e.Seq = "", 0
			replacement = append(replacement, e)
		}
	}
	replacement = append(replacement, compactedEntries...)

	// Either swap is atomic wherever the store can be, so a failed write cannot
	// leave the history cleared but empty.
	g, ok := s.underlying.(session.GuardedReplacer)
	if !ok {
		// A store that cannot compare the log back keeps the unguarded rewrite.
		if err := session.ReplaceEntries(ctx, s.underlying, replacement...); err != nil {
			return fmt.Errorf("compaction: replacing history: %w", err)
		}
		return nil
	}
	replaced, err := g.ReplaceEntriesIf(ctx, expect, replacement...)
	if err != nil {
		return fmt.Errorf("compaction: replacing history: %w", err)
	}
	if !replaced {
		// Something was appended while the compact call was in flight;
		// writing the replacement would delete it.
		span.Set("abandoned", "concurrent_append")
	}
	return nil
}

// isBranched reports whether the history is a tree rather than a line, by the
// one shared branch definition (ActiveBranchOf); leaf moves are excluded.
func isBranched(entries []session.Entry) bool {
	return len(session.ActiveBranchOf(entries)) != len(entries)
}

// ReplaceEntries implements session.AtomicReplacer by delegation only: the
// interface PROMISES atomicity, so a wrapped store that cannot give it is
// refused rather than degraded to Clear+Append.
func (s *CompactionSession) ReplaceEntries(ctx context.Context, entries ...session.Entry) error {
	r, ok := s.underlying.(session.AtomicReplacer)
	if !ok {
		return fmt.Errorf("session storage %T cannot replace entries atomically", s.underlying)
	}
	return r.ReplaceEntries(ctx, entries...)
}

// ReplaceEntriesIf implements session.GuardedReplacer by delegation only: a
// wrapped store without the guard gets an error, not an unguarded rewrite.
func (s *CompactionSession) ReplaceEntriesIf(ctx context.Context, expect int64, entries ...session.Entry) (bool, error) {
	g, ok := s.underlying.(session.GuardedReplacer)
	if !ok {
		return false, fmt.Errorf("session storage %T cannot replace entries under a guard", s.underlying)
	}
	return g.ReplaceEntriesIf(ctx, expect, entries...)
}

// resolveCompactionMode picks how to feed the call: "auto" chains on the last
// stored response unless OffChainItems rule it out; a pinned chain then gets ok=false.
func resolveCompactionMode(configured CompactionMode, args session.CompactionArgs) (mode CompactionMode, ok bool) {
	mode = configured
	if mode == CompactionModeAuto {
		switch {
		case args.Store != nil && !*args.Store:
			mode = CompactionModeInput
		case args.ResponseID == "":
			mode = CompactionModeInput
		default:
			mode = CompactionModePreviousResponseID
		}
	}
	if mode == CompactionModePreviousResponseID && args.OffChainItems {
		if configured == CompactionModePreviousResponseID {
			return mode, false
		}
		return CompactionModeInput, true
	}
	return mode, true
}

// compactionCandidateCount counts items eligible for compaction, excluding user
// messages and existing compaction items.
func compactionCandidateCount(items []agents.InputItem) int {
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

// stripOrphanedAssistantIDs removes assistant message ids when the compacted
// output has no reasoning items: the API rejects an id without its reasoning.
func stripOrphanedAssistantIDs(items []agents.InputItem) []agents.InputItem {
	if len(items) == 0 {
		return items
	}
	for _, it := range items {
		if typ, _, _ := itemShape(it); typ == "reasoning" {
			return items
		}
	}
	out := make([]agents.InputItem, 0, len(items))
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
func itemShape(it agents.InputItem) (typ, role string, hasContent bool) {
	b, err := session.MarshalInputItem(it)
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

// withoutID returns the item with its top-level "id" removed (ok=false if none),
// as a shallow copy so the shared session-history pointee is never mutated.
func withoutID(it agents.InputItem) (agents.InputItem, bool) {
	if it.OfOutputMessage != nil {
		msg := *it.OfOutputMessage
		msg.ID = ""
		return agents.InputItem{OfOutputMessage: &msg}, true
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
