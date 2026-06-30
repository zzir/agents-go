package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
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

// GetItems delegates to the underlying session.
func (s *SlidingWindowSession) GetItems(ctx context.Context, limit int) ([]TResponseInputItem, error) {
	return s.underlying.GetItems(ctx, limit)
}

// AddItems delegates to the underlying session.
func (s *SlidingWindowSession) AddItems(ctx context.Context, items []TResponseInputItem) error {
	return s.underlying.AddItems(ctx, items)
}

// PopItem delegates to the underlying session.
func (s *SlidingWindowSession) PopItem(ctx context.Context) (*TResponseInputItem, error) {
	return s.underlying.PopItem(ctx)
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

	items, err := s.underlying.GetItems(ctx, 0)
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

	toSummarize := items[:len(items)-window]
	toKeep := items[len(items)-window:]

	if IsSingleSummary(toSummarize) {
		return nil
	}

	summaryText, err := s.summarize(ctx, toSummarize)
	if err != nil {
		return fmt.Errorf("sliding window compaction: %w", err)
	}

	summaryItem := responses.ResponseInputItemParamOfMessage(
		SummaryMarker+"\n\n"+summaryText,
		responses.EasyInputMessageRoleSystem,
	)

	replacement := make([]TResponseInputItem, 0, 1+len(toKeep))
	replacement = append(replacement, summaryItem)
	replacement = append(replacement, toKeep...)

	if err := s.underlying.Clear(ctx); err != nil {
		return fmt.Errorf("sliding window compaction: clearing history: %w", err)
	}
	if err := s.underlying.AddItems(ctx, replacement); err != nil {
		return fmt.Errorf("sliding window compaction: writing compacted history: %w", err)
	}
	return nil
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
