package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents"
)

// Strategy shrinks an index. It reports whether it changed anything.
//
// A strategy never deletes: it marks groups excluded, optionally leaving a
// replacement behind. The stored history is the audit trail and stays whole.
type Strategy interface {
	Compact(ctx context.Context, idx *Index) (bool, error)
}

// PipelineStrategy runs strategies in order, stopping at the first one that
// brings the index under budget.
//
// Order is the design: try the cheap, lossless thing first. Folding old tool
// output away costs nothing and loses nothing a model needs; dropping whole
// exchanges loses content; summarizing costs a model call and loses fidelity.
// A pipeline that reaches for the last one first is paying more for a worse
// result.
type PipelineStrategy struct {
	Strategies []Strategy
}

// Compact implements Strategy.
func (p *PipelineStrategy) Compact(ctx context.Context, idx *Index) (bool, error) {
	changed := false
	for _, s := range p.Strategies {
		if s == nil {
			continue
		}
		did, err := s.Compact(ctx, idx)
		if err != nil {
			return changed, err
		}
		changed = changed || did
	}
	return changed, nil
}

// ToolResultStrategy folds old tool-call groups into a compact summary of what
// was called, leaving user messages and assistant prose untouched.
//
// In a coding conversation this is where nearly all the context goes: old file
// reads and command output that mattered for one turn and never again. Folding
// them is cheaper, faster and more faithful than asking a model to summarize
// them, and it needs no model call at all. It is also the only answer to a
// single enormous tool result, where one group IS the overrun.
type ToolResultStrategy struct {
	// Trigger decides when to start; nil never runs.
	Trigger Trigger
	// Target decides when to stop. Nil means "once Trigger stops firing".
	Target Trigger
	// MinimumPreservedGroups keeps this many groups at the end untouched, so
	// the most recent tool results — the ones the model is still working with —
	// survive. Defaults to 2.
	MinimumPreservedGroups int
	// Formatter renders a folded group. Nil uses DefaultToolCallFormatter.
	Formatter func(*Group) string
}

// Compact implements Strategy.
func (s *ToolResultStrategy) Compact(_ context.Context, idx *Index) (bool, error) {
	if !fires(s.Trigger, idx) {
		return false, nil
	}
	preserve := s.MinimumPreservedGroups
	if preserve <= 0 {
		preserve = 2
	}
	format := s.Formatter
	if format == nil {
		format = DefaultToolCallFormatter
	}

	changed := false
	limit := len(idx.Groups) - preserve
	for i, g := range idx.Groups {
		if i >= limit {
			break
		}
		if g.Excluded || g.Kind != GroupToolCall {
			continue
		}
		if reachedTarget(s.Target, s.Trigger, idx) {
			break
		}
		summary := format(g)
		g.Excluded = true
		g.ExcludeReason = "tool_result"
		if summary != "" {
			e, err := foldedEntry(summary)
			if err != nil {
				return changed, err
			}
			g.Replacement = []agents.SessionEntry{e}
		}
		changed = true
	}
	return changed, nil
}

// DefaultToolCallFormatter renders a tool-call group as one line per tool, with
// its results beneath.
//
// It keeps the tool NAMES and drops the payloads. What a model needs from old
// tool calls is usually the fact that they happened and what they were for —
// "I already read that file" — not the bytes.
func DefaultToolCallFormatter(g *Group) string {
	byTool := map[string][]string{}
	var order []string
	for _, e := range g.Entries {
		p := probe(e)
		switch p.Type {
		case "function_call":
			if _, seen := byTool[p.Name]; !seen {
				byTool[p.Name] = nil
				order = append(order, p.Name)
			}
		case "function_call_output":
			// Outputs are keyed by call id, so attribute them to the call's
			// tool name where possible; otherwise group them under the last
			// tool seen, which is right for the common single-tool group.
			name := toolNameForCall(g, p.CallID)
			if name == "" && len(order) > 0 {
				name = order[len(order)-1]
			}
			if name == "" {
				continue
			}
			if _, seen := byTool[name]; !seen {
				order = append(order, name)
			}
			byTool[name] = append(byTool[name], summarizeOutput(p.Output))
		}
	}
	if len(order) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[Tool calls, results elided]\n")
	for _, name := range order {
		fmt.Fprintf(&b, "%s:\n", name)
		results := dedupe(byTool[name])
		if len(results) == 0 {
			b.WriteString("  - (no output recorded)\n")
			continue
		}
		for _, r := range results {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func toolNameForCall(g *Group, callID string) string {
	for _, e := range g.Entries {
		p := probe(e)
		if p.Type == "function_call" && p.CallID == callID {
			return p.Name
		}
	}
	return ""
}

// summarizeOutput reduces a tool result to a single short line. The whole point
// of folding is that the payload is gone, so this describes rather than quotes.
func summarizeOutput(raw []byte) string {
	const maxChars = 120
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "(empty)"
	}
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + fmt.Sprintf("… (%d chars elided)", len(s)-maxChars)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// foldedEntry wraps folded text as a system message: the runtime is saying what
// happened, and attributing it to the user or the assistant would put words in
// someone's mouth.
func foldedEntry(text string) (agents.SessionEntry, error) {
	items := agents.InputItemsFromSystemText(text)
	if len(items) == 0 {
		return agents.SessionEntry{}, fmt.Errorf("compaction: empty folded entry")
	}
	return agents.NewItemEntry(items[0], agents.Source{Type: agents.SourceCompaction})
}

// TruncationStrategy drops whole groups from the oldest end.
//
// It is the blunt instrument: it loses content outright, so it belongs after
// tool folding in a pipeline, not before.
type TruncationStrategy struct {
	Trigger Trigger
	Target  Trigger
	// MinimumPreservedGroups keeps this many groups at the end. Defaults to 2.
	MinimumPreservedGroups int
	// PreserveSystem keeps system groups regardless of age, since instructions
	// apply to the whole conversation rather than to the turn that carried
	// them. Defaults to true.
	PreserveSystem *bool
}

// Compact implements Strategy.
func (s *TruncationStrategy) Compact(_ context.Context, idx *Index) (bool, error) {
	if !fires(s.Trigger, idx) {
		return false, nil
	}
	preserve := s.MinimumPreservedGroups
	if preserve <= 0 {
		preserve = 2
	}
	keepSystem := s.PreserveSystem == nil || *s.PreserveSystem

	changed := false
	limit := len(idx.Groups) - preserve
	for i, g := range idx.Groups {
		if i >= limit || reachedTarget(s.Target, s.Trigger, idx) {
			break
		}
		if g.Excluded {
			continue
		}
		if keepSystem && g.Kind == GroupSystem {
			continue
		}
		g.Excluded = true
		g.ExcludeReason = "truncation"
		changed = true
	}
	return changed, nil
}

// ContextWindowStrategy derives its thresholds from the model's own limits, so
// a caller does not have to guess numbers that depend on the model anyway.
//
// It runs two stages: fold tool results at half the input budget, drop whole
// groups at four fifths. Cheap first, lossy last.
type ContextWindowStrategy struct {
	// MaxContextWindowTokens is the model's context window.
	MaxContextWindowTokens int
	// MaxOutputTokens is what must stay free for the answer.
	MaxOutputTokens int
	// ToolEvictionThreshold is the fraction of the input budget at which tool
	// results start folding. Defaults to 0.5.
	ToolEvictionThreshold float64
	// TruncationThreshold is the fraction at which whole groups start dropping.
	// Defaults to 0.8.
	TruncationThreshold float64
	// MinimumPreservedGroups is passed to both stages. Defaults to 2.
	MinimumPreservedGroups int
}

// Compact implements Strategy.
func (s *ContextWindowStrategy) Compact(ctx context.Context, idx *Index) (bool, error) {
	budget := s.MaxContextWindowTokens - s.MaxOutputTokens
	if budget <= 0 {
		return false, fmt.Errorf("compaction: context window %d leaves no room for %d output tokens",
			s.MaxContextWindowTokens, s.MaxOutputTokens)
	}
	evict := s.ToolEvictionThreshold
	if evict <= 0 {
		evict = 0.5
	}
	truncate := s.TruncationThreshold
	if truncate <= 0 {
		truncate = 0.8
	}

	pipeline := &PipelineStrategy{Strategies: []Strategy{
		&ToolResultStrategy{
			Trigger:                TokensExceed(int(float64(budget) * evict)),
			MinimumPreservedGroups: s.MinimumPreservedGroups,
		},
		&TruncationStrategy{
			Trigger:                TokensExceed(int(float64(budget) * truncate)),
			MinimumPreservedGroups: s.MinimumPreservedGroups,
		},
	}}
	return pipeline.Compact(ctx, idx)
}
