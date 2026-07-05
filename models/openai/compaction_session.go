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
// It is the Go counterpart of Python's OpenAIResponsesCompactionSession.
//
// Unlike Python, which can compact after every turn, the Go runner persists a
// run's items once at completion, so compaction is attempted once per run (when
// the run finishes and its items are saved). The decision hook still bounds how
// often the responses.compact API is actually called.
type CompactionSession struct {
	underlying agents.Session
	svc        responses.ResponseService
	model      string
	mode       CompactionMode
	threshold  int
	should     func(int) bool
}

var (
	_ agents.Session                = (*CompactionSession)(nil)
	_ agents.CompactionAwareSession = (*CompactionSession)(nil)
)

// NewCompactionSession wraps underlying with automatic responses.compact
// compaction. Pass openai-go request options (option.WithAPIKey, …) just as for
// NewProvider. It rejects wrapping a ConversationsSession (which manages its own
// server-side history) and a non-OpenAI compaction model.
func NewCompactionSession(underlying agents.Session, opts CompactionOptions, clientOpts ...option.RequestOption) (*CompactionSession, error) {
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
		threshold:  threshold,
		should:     should,
	}, nil
}

// GetItems implements agents.Session.
func (s *CompactionSession) GetItems(ctx context.Context, limit int) ([]agents.TResponseInputItem, error) {
	return s.underlying.GetItems(ctx, limit)
}

// AddItems implements agents.Session.
func (s *CompactionSession) AddItems(ctx context.Context, items []agents.TResponseInputItem) error {
	return s.underlying.AddItems(ctx, items)
}

// PopItem implements agents.Session.
func (s *CompactionSession) PopItem(ctx context.Context) (*agents.TResponseInputItem, error) {
	return s.underlying.PopItem(ctx)
}

// Clear implements agents.Session.
func (s *CompactionSession) Clear(ctx context.Context) error {
	return s.underlying.Clear(ctx)
}

// RunCompaction implements agents.CompactionAwareSession. It compacts the stored
// history via responses.compact when the decision hook (or args.Force) says so,
// replacing the underlying session's items with the compacted output.
func (s *CompactionSession) RunCompaction(ctx context.Context, args agents.CompactionArgs) error {
	items, err := s.underlying.GetItems(ctx, 0)
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

	// ReplaceSessionItems swaps atomically when the underlying session supports
	// it, so a failed write cannot leave the history cleared but empty.
	if err := agents.ReplaceSessionItems(ctx, s.underlying, out); err != nil {
		return fmt.Errorf("compaction: replacing history: %w", err)
	}
	return nil
}

// resolveCompactionMode mirrors Python's _resolve_compaction_mode: under "auto",
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
// messages and existing compaction items (matching Python's
// select_compaction_candidate_items).
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
// without its paired reasoning item is rejected by the API (matching Python's
// _strip_orphaned_assistant_ids).
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
// has none.
func withoutID(it agents.TResponseInputItem) (agents.TResponseInputItem, bool) {
	b, err := agents.MarshalInputItem(it)
	if err != nil {
		return it, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return it, false
	}
	if _, ok := m["id"]; !ok {
		return it, false
	}
	delete(m, "id")
	nb, err := json.Marshal(m)
	if err != nil {
		return it, false
	}
	cleaned, err := agents.UnmarshalInputItem(nb)
	if err != nil {
		return it, false
	}
	return cleaned, true
}

// isOpenAIModelName mirrors Python's is_openai_model_name: gpt-*, o<digit>*, or
// ft:gpt-* prefixes.
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
