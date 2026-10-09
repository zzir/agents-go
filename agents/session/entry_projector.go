package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

// Projector turns a session entry into the model input items it contributes;
// none means the model does not see the entry.
type Projector func(Entry) ([]InputItem, error)

// defaultProjectors is the projection every run starts from: items only.
// Checkpoints are absent on purpose — ProjectEntries applies them structurally.
var defaultProjectors = map[EntryKind]Projector{
	EntryKindItem: projectItem,
}

func projectItem(e Entry) ([]InputItem, error) {
	item, err := e.InputItem()
	if err != nil {
		return nil, err
	}
	return []InputItem{item}, nil
}

// SummaryMarker prefixes a compaction summary so a later pass does not
// summarize it again.
const SummaryMarker = "[Conversation Summary]"

// DefaultSummaryPrompt is the system prompt a compaction pass summarizes a
// transcript with.
var DefaultSummaryPrompt = strings.TrimSpace(`
You are a conversation summarizer. You will receive a plain-text transcript
of a portion of a conversation between a user and an AI assistant. Summarize
it into a concise factual account that preserves:
- Key decisions and conclusions
- Important facts, names, numbers, and code identifiers mentioned
- The current state of any ongoing task
- Any commitments or action items

Be concise but complete. Do not add commentary. Do not invent information.
Write plain prose only: never emit tool-call syntax, function-call markup, or
model control tokens — describe in words what a tool call did instead. The
summary is injected into future conversations as text, where such markup would
read as instructions rather than history.
Output only the summary text.
`)

// CompactionFold is one folded group's stand-in, rendered in the group's place.
// Its content is original; folded entries are named (Replaces), never copied —
// spec §2.5f.
type CompactionFold struct {
	// Replaces names the folded entries this stand-in renders instead of.
	Replaces []string `json:"replaces,omitzero"`
	// Before anchors the stand-in: it renders immediately before this entry.
	// Empty, or an id absent from the view, renders it up front instead.
	Before string `json:"before,omitzero"`
	// Items are the stand-in's input items, in order.
	Items []json.RawMessage `json:"items,omitzero"`
}

// CompactionPayload is the body of a compaction checkpoint: what a pass folded
// away (by name) and what stands in for it. It copies no entry — spec §2.5f.
type CompactionPayload struct {
	// Summary is the text that stands in for the folded history; it renders up front.
	Summary string `json:"summary"`
	// Folds are per-group stand-ins, anchored where the folded group was.
	Folds []CompactionFold `json:"folds,omitzero"`
	// PrevSummary is the summary this one supersedes, when there is one.
	PrevSummary string `json:"prev_summary,omitzero"`
	// ExcludedIDs names the entries this checkpoint folded away; they stay in
	// the session.
	ExcludedIDs []string `json:"excluded_ids,omitzero"`
	// TokensBefore and TokensAfter estimate the context on either side of the pass.
	TokensBefore int `json:"tokens_before,omitzero"`
	TokensAfter  int `json:"tokens_after,omitzero"`
	// Reset marks a pass that folded the conversation rather than summarizing it;
	// Summary is then what the model kept for itself — spec §2.5i.
	Reset bool `json:"reset,omitzero"`
}

// CompactionPayload decodes a compaction checkpoint's payload.
func (e Entry) CompactionPayload() (CompactionPayload, error) {
	if e.Kind != EntryKindCompaction {
		return CompactionPayload{}, fmt.Errorf("entry %q is a %s entry, not a compaction checkpoint", e.ID, e.Kind)
	}
	var p CompactionPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return CompactionPayload{}, fmt.Errorf("decoding compaction payload for entry %q: %w", e.ID, err)
	}
	return p, nil
}

// FoldedEntryIDs collects every entry id folded by ANY compaction checkpoint in
// entries, a later-folded checkpoint's included; an undecodable one adds nothing.
func FoldedEntryIDs(entries []Entry) map[string]bool {
	var folded map[string]bool
	for _, e := range entries {
		if e.Kind != EntryKindCompaction {
			continue
		}
		p, err := e.CompactionPayload()
		if err != nil {
			continue
		}
		for _, id := range p.ExcludedIDs {
			if folded == nil {
				folded = make(map[string]bool)
			}
			folded[id] = true
		}
	}
	return folded
}

// projectorFor resolves the projector for a kind; a caller's override wins over
// the default.
func projectorFor(overrides map[EntryKind]Projector, kind EntryKind) (Projector, bool) {
	if p, ok := overrides[kind]; ok {
		return p, p != nil
	}
	p, ok := defaultProjectors[kind]
	return p, ok
}

// ProjectEntries turns one branch's entries into model input: updates fold into
// their targets, and each live checkpoint drops its ExcludedIDs and renders its
// summary and folds; an EntryKindCompaction override takes over only the
// rendering — spec §2.5c.
func ProjectEntries(entries []Entry, overrides map[EntryKind]Projector) ([]InputItem, error) {
	folded := FoldedEntryIDs(entries)
	_, checkpointOverridden := overrides[EntryKindCompaction]

	// The render plan: summaries and anchorless stand-ins up front, anchored ones
	// keyed by the entry they precede. Only live checkpoints render.
	var front []InputItem
	var inserts map[string][]InputItem
	if !checkpointOverridden {
		present := make(map[string]bool, len(entries))
		for _, e := range entries {
			present[e.ID] = true
		}
		for _, e := range entries {
			if e.Kind != EntryKindCompaction || folded[e.ID] {
				continue
			}
			p, err := e.CompactionPayload()
			if err != nil {
				return nil, err
			}
			if p.Summary != "" {
				// A system message, never a user one — spec §2.5b.
				front = append(front, systemTextItems(p.Summary)...)
			}
			for fi, f := range p.Folds {
				items := make([]InputItem, 0, len(f.Items))
				for i, raw := range f.Items {
					item, uerr := UnmarshalInputItem(raw)
					if uerr != nil {
						return nil, fmt.Errorf("decoding fold %d item %d of entry %q: %w", fi, i, e.ID, uerr)
					}
					items = append(items, item)
				}
				if f.Before != "" && present[f.Before] {
					if inserts == nil {
						inserts = make(map[string][]InputItem)
					}
					inserts[f.Before] = append(inserts[f.Before], items...)
				} else {
					// Anchor absent from this view: front the stand-in, its content
					// over its position.
					front = append(front, items...)
				}
			}
		}
	}

	out := make([]InputItem, 0, len(front)+len(entries))
	out = append(out, front...)
	for _, e := range entries {
		if ins, ok := inserts[e.ID]; ok {
			out = append(out, ins...)
		}
		if folded[e.ID] || e.Kind == EntryKindUpdate {
			continue
		}
		if e.Kind == EntryKindCompaction && !checkpointOverridden {
			continue
		}
		project, ok := projectorFor(overrides, e.Kind)
		if !ok {
			// A kind nobody projects (an annotation, an unknown kind) is
			// skipped, not an error — spec §2.5b.
			continue
		}
		items, err := project(e)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// FoldUpdates applies every update entry to its target and drops the updates.
// Updates apply in store order, last write wins per field; an update whose
// target is missing is ignored (spec §2.5b).
func FoldUpdates(entries []Entry) []Entry {
	// Index targets first so an update that precedes its target still applies.
	index := make(map[string]int, len(entries))
	byCall := make(map[string]int)
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Kind == EntryKindUpdate {
			continue
		}
		if e.ID != "" {
			index[e.ID] = len(out)
		}
		// A tool call is also addressable by its call id — spec §2.5b.
		if e.Display != nil && e.Display.CallID != "" && e.Display.Kind == DisplayToolCall {
			byCall[e.Display.CallID] = len(out)
		}
		out = append(out, e)
	}

	for _, e := range entries {
		if e.Kind != EntryKindUpdate {
			continue
		}
		p, err := e.UpdatePayload()
		if err != nil {
			continue // an undecodable update amends nothing
		}
		i, ok := index[p.TargetID]
		if !ok && p.TargetCallID != "" {
			i, ok = byCall[p.TargetCallID]
		}
		if !ok {
			continue
		}
		merged := ItemDisplay{}
		if out[i].Display != nil {
			merged = *out[i].Display
		}
		merged.merge(p.Display)
		out[i].Display = &merged
	}
	return out
}

// NewCompactionEntry builds a compaction checkpoint from what a pass folded
// away; the entries it kept stay in the session and are no part of it.
func NewCompactionEntry(p CompactionPayload) (Entry, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return Entry{}, fmt.Errorf("encoding compaction payload: %w", err)
	}
	return Entry{
		Kind:    EntryKindCompaction,
		Source:  Source{Type: SourceCompaction},
		Payload: raw,
	}, nil
}

// ExtractOutputText returns the first output_text in a model response's output,
// "" when there is none; a compaction call reads its summary through it.
func ExtractOutputText(output []OutputItem) string {
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

// systemTextItems builds one system message: the runtime speaking in place of
// folded history.
func systemTextItems(text string) []InputItem {
	return []InputItem{
		responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleSystem),
	}
}
