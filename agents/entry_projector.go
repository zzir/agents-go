package agents

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EntryProjector turns a session entry into the model input items it
// contributes. Returning none means the entry is not part of the conversation
// the model sees.
//
// It is the single place that answers "what does the model get to read". That
// question used to be answered implicitly — by what a session happened to
// store — so anything worth keeping but not worth sending had no home.
type EntryProjector func(SessionEntry) ([]TResponseInputItem, error)

// defaultProjectors is the projection every run starts from.
//
// Only items and compaction checkpoints reach the model. Annotations, terminal
// output and custom entries are recorded for people, and sending them would put
// words in the conversation that nobody said. A caller who wants them in
// context says so explicitly through RunOptions.Conversation.Projectors — for
// example projecting terminal output as a user message so the model can see
// what was run by hand.
var defaultProjectors = map[EntryKind]EntryProjector{
	EntryKindItem:       projectItem,
	EntryKindCompaction: projectCompaction,
}

func projectItem(e SessionEntry) ([]TResponseInputItem, error) {
	item, err := e.InputItem()
	if err != nil {
		return nil, err
	}
	return []TResponseInputItem{item}, nil
}

// SummaryMarker prefixes a compaction summary. It is how a later pass
// recognizes an existing summary and refuses to summarize it again, which is
// what stops a long conversation from decaying into a summary of a summary of
// a summary.
const SummaryMarker = "[Conversation Summary]"

// DefaultSummaryPrompt is the default system prompt used to summarize
// conversation history during compaction.
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

// CompactionPayload is the body of a compaction checkpoint: a summary of what
// was folded away, plus the entries kept verbatim after it.
//
// The retained tail lives inside the checkpoint rather than being left loose in
// the session, so the checkpoint is self-contained: reading it gives the whole
// context that replaced the history it summarizes, with no separate range to
// track.
//
// A checkpoint is APPENDED, never a rewrite. The entries it folds stay in the
// session exactly as they were — ExcludedIDs names them, so a UI can offer to
// expand what was compacted, and a fork from before the checkpoint still finds
// its full history.
type CompactionPayload struct {
	// Summary is the text that stands in for the folded history.
	Summary string `json:"summary"`
	// Retained are the items kept verbatim after the summary.
	Retained []json.RawMessage `json:"retained,omitzero"`
	// PrevSummary is the summary this one supersedes, when a checkpoint
	// updates an earlier one rather than starting fresh.
	PrevSummary string `json:"prev_summary,omitzero"`
	// ExcludedIDs are the entries this checkpoint folded away. They are still
	// in the session; this is what lets a reader offer them back.
	ExcludedIDs []string `json:"excluded_ids,omitzero"`
	// TokensBefore and TokensAfter estimate the context on either side of the
	// pass, so a session can report what compaction bought without recomputing
	// it.
	TokensBefore int `json:"tokens_before,omitzero"`
	TokensAfter  int `json:"tokens_after,omitzero"`
}

// CompactionPayload decodes a compaction checkpoint's payload.
func (e SessionEntry) CompactionPayload() (CompactionPayload, error) {
	if e.Kind != EntryKindCompaction {
		return CompactionPayload{}, fmt.Errorf("entry %q is a %s entry, not a compaction checkpoint", e.ID, e.Kind)
	}
	var p CompactionPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return CompactionPayload{}, fmt.Errorf("decoding compaction payload for entry %q: %w", e.ID, err)
	}
	return p, nil
}

func projectCompaction(e SessionEntry) ([]TResponseInputItem, error) {
	p, err := e.CompactionPayload()
	if err != nil {
		return nil, err
	}
	out := make([]TResponseInputItem, 0, len(p.Retained)+1)
	if p.Summary != "" {
		// A system message, not a user one: nobody said this. It is context the
		// runtime supplies in place of history it folded away.
		out = append(out, InputItemsFromSystemText(p.Summary)...)
	}
	for i, raw := range p.Retained {
		item, err := UnmarshalInputItem(raw)
		if err != nil {
			return nil, fmt.Errorf("decoding retained item %d of entry %q: %w", i, e.ID, err)
		}
		out = append(out, item)
	}
	return out, nil
}

// projectorFor resolves the projector for a kind, letting a caller's overrides
// win over the defaults.
func projectorFor(overrides map[EntryKind]EntryProjector, kind EntryKind) (EntryProjector, bool) {
	if p, ok := overrides[kind]; ok {
		return p, p != nil
	}
	p, ok := defaultProjectors[kind]
	return p, ok
}

// ProjectEntries turns a session's entries into the model input for a run.
//
// Update entries are folded into their targets rather than projected: an update
// amends a display, and displays are not part of what the model reads.
func ProjectEntries(entries []SessionEntry, overrides map[EntryKind]EntryProjector) ([]TResponseInputItem, error) {
	out := make([]TResponseInputItem, 0, len(entries))
	for _, e := range entries {
		if e.Kind == EntryKindUpdate {
			continue
		}
		project, ok := projectorFor(overrides, e.Kind)
		if !ok {
			// A kind nobody projects — an annotation, or one this build does
			// not know. Not an error: the entry was recorded for someone else.
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

// FoldUpdates applies every update entry to its target and drops the updates,
// returning the entries a consumer should render.
//
// Two rules, both load-bearing:
//
//   - Updates apply in the order they were stored, so the last write wins per
//     field.
//   - An update whose target is missing is IGNORED, not an error. The target
//     may have been folded away by compaction, or may simply not have been
//     written yet — an update is allowed to arrive first, which is what removes
//     the ordering race instead of handling it.
func FoldUpdates(entries []SessionEntry) []SessionEntry {
	// Index targets first so an update that precedes its target still applies.
	index := make(map[string]int, len(entries))
	byCall := make(map[string]int)
	out := make([]SessionEntry, 0, len(entries))
	for _, e := range entries {
		if e.Kind == EntryKindUpdate {
			continue
		}
		if e.ID != "" {
			index[e.ID] = len(out)
		}
		// A tool call is also addressable by its call id, for an amender that
		// knows the call and not the entry.
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

// NewCompactionEntry builds a compaction checkpoint. retained are the items
// kept verbatim after the summary; the rest of the payload describes what the
// pass folded away.
func NewCompactionEntry(p CompactionPayload, retained []TResponseInputItem) (SessionEntry, error) {
	for i, item := range retained {
		raw, err := MarshalInputItem(item)
		if err != nil {
			return SessionEntry{}, fmt.Errorf("encoding retained item %d: %w", i, err)
		}
		p.Retained = append(p.Retained, raw)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return SessionEntry{}, fmt.Errorf("encoding compaction payload: %w", err)
	}
	return SessionEntry{
		Kind:    EntryKindCompaction,
		Source:  Source{Type: SourceCompaction},
		Payload: raw,
	}, nil
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
