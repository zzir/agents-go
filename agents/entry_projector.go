package agents

import (
	"encoding/json"
	"fmt"
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

// CompactionPayload is the body of a compaction checkpoint: a summary of what
// was folded away, plus the entries kept verbatim after it.
//
// The retained tail lives inside the checkpoint rather than being left loose in
// the session, so the checkpoint is self-contained: reading it gives the whole
// context that replaced the history it summarizes, with no separate range to
// track.
type CompactionPayload struct {
	// Summary is the text that stands in for the folded history.
	Summary string `json:"summary"`
	// Retained are the items kept verbatim after the summary.
	Retained []json.RawMessage `json:"retained,omitzero"`
}

func projectCompaction(e SessionEntry) ([]TResponseInputItem, error) {
	var p CompactionPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fmt.Errorf("decoding compaction payload for entry %q: %w", e.ID, err)
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
	out := make([]SessionEntry, 0, len(entries))
	for _, e := range entries {
		if e.Kind == EntryKindUpdate {
			continue
		}
		if e.ID != "" {
			index[e.ID] = len(out)
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

// newCompactionEntry builds a compaction checkpoint from a summary and the
// items kept verbatim after it.
func newCompactionEntry(summary string, retained []TResponseInputItem) (SessionEntry, error) {
	payload := CompactionPayload{Summary: summary}
	for i, item := range retained {
		raw, err := MarshalInputItem(item)
		if err != nil {
			return SessionEntry{}, fmt.Errorf("encoding retained item %d: %w", i, err)
		}
		payload.Retained = append(payload.Retained, raw)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return SessionEntry{}, fmt.Errorf("encoding compaction payload: %w", err)
	}
	return SessionEntry{
		Kind:    EntryKindCompaction,
		Source:  Source{Type: SourceCompaction},
		Payload: raw,
	}, nil
}
