package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/tracing"
)

const (
	defaultSlidingWindowThreshold = 20
	defaultSlidingWindowSize      = 10

	// SummaryMarker is the prefix for compaction summary messages, used to
	// detect existing summaries and avoid summary-of-summary loops.
	SummaryMarker = "[Conversation Summary]"
)

// DefaultSummaryPrompt is the default system prompt used to summarize
// conversation history during sliding-window compaction.
var DefaultSummaryPrompt = strings.TrimSpace(`
You are a conversation summarizer. You will receive a portion of a
conversation between a user and an AI assistant. Summarize it into a
concise factual account that preserves:
- Key decisions and conclusions
- Important facts, names, numbers, and code identifiers mentioned
- The current state of any ongoing task
- Any commitments or action items

Be concise but complete. Do not add commentary. Do not invent information.
Output only the summary text.
`)

// SlidingWindowConfig configures a SlidingWindowSession.
type SlidingWindowConfig struct {
	// Threshold is the number of items beyond the window that triggers
	// compaction. For example, with Threshold=6 and WindowSize=3,
	// compaction fires when there are 9+ total items (6 outside the window).
	// Default: 20.
	Threshold int
	// WindowSize is how many recent items to keep intact (not summarized).
	// Default: 10.
	WindowSize int
	// SummaryModel is the Model used to generate conversation summaries.
	SummaryModel Model
	// SummaryPrompt overrides the default system instructions sent to
	// SummaryModel when generating a summary.
	SummaryPrompt string
	// ShouldCompact overrides the default threshold check. It receives the
	// total number of stored items and returns true when compaction should run.
	ShouldCompact func(totalItems int) bool
}

// SlidingWindowSession decorates any Session with provider-agnostic
// compaction: when history grows past a threshold, older items beyond the
// sliding window are summarized by an LLM and replaced with a single
// summary message.
//
// Concurrency follows the Session contract, which does not promise safety for
// concurrent use of one instance: every method here (including RunCompaction's
// GetItems -> summarize -> ReplaceSessionItems read-modify-write) assumes serial
// access to a given session, exactly as the runner drives it — one run at a
// time, with compaction run only after that run's history has been persisted.
// Sharing a single SlidingWindowSession across concurrent runs would race on the
// underlying session's own state regardless of any lock added here, so callers
// that need that must serialize access themselves (or give each run its own
// session). No lock is taken; holding one across the summarization model call
// would serialize unrelated session operations behind a network round-trip.
type SlidingWindowSession struct {
	underlying Session
	cfg        SlidingWindowConfig
}

var (
	_ Session                = (*SlidingWindowSession)(nil)
	_ CompactionAwareSession = (*SlidingWindowSession)(nil)
)

// NewSlidingWindowSession wraps underlying with sliding-window compaction.
func NewSlidingWindowSession(underlying Session, cfg SlidingWindowConfig) *SlidingWindowSession {
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultSlidingWindowThreshold
	}
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = defaultSlidingWindowSize
	}
	if cfg.ShouldCompact == nil {
		threshold := cfg.Threshold
		window := cfg.WindowSize
		cfg.ShouldCompact = func(n int) bool { return n-window >= threshold }
	}
	if cfg.SummaryPrompt == "" {
		cfg.SummaryPrompt = DefaultSummaryPrompt
	}
	return &SlidingWindowSession{underlying: underlying, cfg: cfg}
}

// GetEntries delegates to the underlying session.
func (s *SlidingWindowSession) GetEntries(ctx context.Context, limit int) ([]SessionEntry, error) {
	return s.underlying.GetEntries(ctx, limit)
}

// AddEntries delegates to the underlying session.
func (s *SlidingWindowSession) AddEntries(ctx context.Context, entries []SessionEntry) error {
	return s.underlying.AddEntries(ctx, entries)
}

// PopEntry delegates to the underlying session.
func (s *SlidingWindowSession) PopEntry(ctx context.Context) (*SessionEntry, error) {
	return s.underlying.PopEntry(ctx)
}

// Clear delegates to the underlying session.
func (s *SlidingWindowSession) Clear(ctx context.Context) error {
	return s.underlying.Clear(ctx)
}

// RunCompaction implements CompactionAwareSession. It summarizes older
// items via SummaryModel and replaces them with a single summary message,
// keeping the most recent WindowSize items intact.
func (s *SlidingWindowSession) RunCompaction(ctx context.Context, args CompactionArgs) error {
	if s.cfg.SummaryModel == nil {
		return nil
	}

	entries, err := s.underlying.GetEntries(ctx, 0)
	if err != nil {
		return err
	}
	// Compaction works on what the model reads, so it starts from the
	// projection rather than the raw entries: an annotation is not context and
	// must not be summarized as if it were.
	items, err := ProjectEntries(entries, nil)
	if err != nil {
		return err
	}

	if !args.Force && !s.cfg.ShouldCompact(len(items)) {
		return nil
	}

	window := s.cfg.WindowSize
	if window >= len(items) {
		return nil
	}

	// A pure count-based split can cut through a function_call / output pair
	// or detach a reasoning item from its successor, producing sequences the
	// Responses API rejects. Move the split so both sides stay self-consistent.
	split := SafeSplitPoint(items, len(items)-window)
	if split <= 0 {
		// No self-consistent, non-empty prefix exists (the adjustment pulled
		// everything into the keep side). Skip compaction rather than risk
		// corrupting the history.
		return nil
	}
	toSummarize := items[:split]
	toKeep := items[split:]

	if IsSingleSummary(toSummarize) {
		return nil
	}

	var span *tracing.SpanHandle
	if args.StartSpan != nil {
		span = args.StartSpan()
	}
	span.Set("before_items", len(items))
	span.Set("after_items", 1+len(toKeep))

	summaryText, err := s.summarize(ctx, toSummarize)
	if err != nil {
		return fmt.Errorf("sliding window compaction: %w", err)
	}
	if summaryText == "" {
		// The model returned no usable text (e.g. reasoning or refusal only).
		// Rewriting history around an empty summary would silently discard the
		// summarized items, so fail loudly and leave the session untouched.
		return fmt.Errorf("sliding window compaction: summary model returned no output text")
	}

	// The result is ONE compaction checkpoint carrying the summary and the tail
	// it kept. Keeping the retained items inside the checkpoint makes it
	// self-contained: reading it gives the whole context that replaced the
	// history it folded away, with no separate range to track.
	checkpoint, err := newCompactionEntry(SummaryMarker+"\n\n"+summaryText, toKeep)
	if err != nil {
		return fmt.Errorf("sliding window compaction: %w", err)
	}
	if err := ReplaceSessionEntries(ctx, s.underlying, []SessionEntry{checkpoint}); err != nil {
		return fmt.Errorf("sliding window compaction: replacing history: %w", err)
	}
	return nil
}

// SafeSplitPoint refines an initial count-based split index so that both
// items[:split] (summarized away) and items[split:] (kept verbatim) remain
// self-consistent Responses sequences:
//
// - a function_call and its function_call_output (paired by call_id) must
// land on the same side — splitting them makes the summarization request
// itself invalid, or leaves the rewritten history beginning with an
// orphaned function_call_output, and either way the next Run is rejected;
// - a reasoning item must stay with its successor (the message or
// function_call it precedes), so it cannot be the final prefix item.
//
// The split only ever moves toward keeping more items (it decreases): an
// offending pair is kept whole on the keep side. It returns 0 when no valid
// non-empty prefix exists, in which case the caller should skip compaction.
// Only function tool pairs need handling here: the SDK executes function
// tools exclusively, so histories contain no other call/output item kinds.
//
// It is exported for external Session implementations that rewrite history
// (compaction, summarization, forking) and need the same pair-safety
// guarantee; SlidingWindowSession uses it internally.
func SafeSplitPoint(items []TResponseInputItem, split int) int {
	if split < 0 {
		return 0
	}
	if split > len(items) {
		split = len(items)
	}
	for split > 0 {
		moved := false

		// Keep reasoning items with their successor: a reasoning item may not
		// end the prefix while the item it belongs to starts the suffix.
		for split > 0 && items[split-1].OfReasoning != nil {
			split--
			moved = true
		}

		// Keep function_call / function_call_output pairs on one side. When a
		// pair straddles the split, pull the whole pair into the keep side.
		if split > 0 {
			if idx := earliestStraddlingPair(items, split); idx >= 0 {
				split = idx
				moved = true
			}
		}

		if !moved {
			break
		}
	}
	return split
}

// earliestStraddlingPair returns the smallest index of a function_call /
// function_call_output item whose call_id partner sits on the other side of
// split, or -1 when no pair straddles the split. Items with a missing
// counterpart (already-orphaned history) never straddle: their single index
// is always on one side.
func earliestStraddlingPair(items []TResponseInputItem, split int) int {
	firstIdx := make(map[string]int)
	lastIdx := make(map[string]int)
	for i := range items {
		var callID string
		switch {
		case items[i].OfFunctionCall != nil:
			callID = items[i].OfFunctionCall.CallID
		case items[i].OfFunctionCallOutput != nil:
			callID = items[i].OfFunctionCallOutput.CallID
		default:
			continue
		}
		if callID == "" {
			continue
		}
		if _, ok := firstIdx[callID]; !ok {
			firstIdx[callID] = i
		}
		lastIdx[callID] = i
	}
	best := -1
	for callID, lo := range firstIdx {
		if hi := lastIdx[callID]; lo < split && hi >= split {
			if best == -1 || lo < best {
				best = lo
			}
		}
	}
	return best
}

func (s *SlidingWindowSession) summarize(ctx context.Context, items []TResponseInputItem) (string, error) {
	resp, err := s.cfg.SummaryModel.GetResponse(ctx, ModelRequest{
		SystemInstructions: s.cfg.SummaryPrompt,
		Input:              items,
	})
	if err != nil {
		return "", err
	}
	return ExtractOutputText(resp.Output), nil
}

// ExtractOutputText returns the first output_text content from a model
// response output. Used to extract summary text from a compaction call.
func ExtractOutputText(output []TResponseOutputItem) string {
	for _, item := range output {
		b := []byte(item.RawJSON())
		var probe struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(b, &probe) == nil {
			for _, c := range probe.Content {
				if c.Type == "output_text" && c.Text != "" {
					return c.Text
				}
			}
		}
	}
	return ""
}

// IsSingleSummary returns true when the prefix consists of exactly one
// item that is already a conversation summary, avoiding summary-of-summary
// loops.
func IsSingleSummary(items []TResponseInputItem) bool {
	if len(items) != 1 {
		return false
	}
	b, err := MarshalInputItem(items[0])
	if err != nil {
		return false
	}
	var probe struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(b, &probe) == nil && probe.Role == "system" && strings.HasPrefix(probe.Content, SummaryMarker) {
		return true
	}
	var parts struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(b, &parts) == nil && parts.Role == "system" {
		for _, p := range parts.Content {
			if (p.Type == "input_text" || p.Type == "output_text") && strings.HasPrefix(p.Text, SummaryMarker) {
				return true
			}
		}
	}
	return false
}
