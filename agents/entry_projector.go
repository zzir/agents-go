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
type EntryProjector func(SessionEntry) ([]InputItem, error)

// defaultProjectors is the projection every run starts from.
//
// Only items reach the model this way. Annotations, terminal output and custom
// entries are recorded for people, and sending them would put words in the
// conversation that nobody said. A caller who wants them in context says so
// explicitly through RunOptions.Conversation.Projectors — for example
// projecting terminal output as a user message so the model can see what was
// run by hand.
//
// Compaction checkpoints are absent deliberately: a checkpoint is not one
// entry's worth of items, it is a rule about the whole view — drop what it
// folded, render its summary and stand-ins where the folded content was —
// and ProjectEntries applies that rule structurally. An override for
// EntryKindCompaction disables the structural handling and takes over.
var defaultProjectors = map[EntryKind]EntryProjector{
	EntryKindItem: projectItem,
}

func projectItem(e SessionEntry) ([]InputItem, error) {
	item, err := e.InputItem()
	if err != nil {
		return nil, err
	}
	return []InputItem{item}, nil
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

// CompactionFold is one folded group's stand-in: content that renders in place
// of a run of folded entries — "[Tool calls, results elided]", say.
//
// The stand-in is ORIGINAL content, existing nowhere else in the log, which is
// why it may live inside the checkpoint. Entries that are still in the session
// are only ever NAMED (Replaces, ExcludedIDs), never copied: a copy has to be
// kept in step with the entry it duplicates, and the one that got out of step
// is what this shape replaced.
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
// away (by name) and what stands in for it (summary text, per-group folds).
//
// A checkpoint carries no copy of any entry that is still in the session. It
// used to — a Retained field held the kept tail verbatim — and that copy could
// only ever drift from the tree it duplicated: popping a kept entry left the
// checkpoint still replaying it. The tree is the truth; ProjectEntries applies
// a checkpoint's exclusions when the model's view is built.
//
// A checkpoint is APPENDED, never a rewrite. The entries it folds stay in the
// session exactly as they were — ExcludedIDs names them, so a UI can offer to
// expand what was compacted, a fork from before the checkpoint still finds its
// full history, and popping the checkpoint itself undoes the fold.
type CompactionPayload struct {
	// Summary is the text that stands in for the folded history. It renders at
	// the front of the projection, before everything the pass kept.
	Summary string `json:"summary"`
	// Folds are per-group stand-ins, anchored where the folded group was.
	Folds []CompactionFold `json:"folds,omitzero"`
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

// FoldedEntryIDs collects every entry id the given entries' compaction
// checkpoints have folded away — the union over ALL checkpoints present, not
// just the newest. Exclusion is forever: a checkpoint that is itself later
// folded still keeps what IT folded out of view, or every second-generation
// pass would resurrect the first one's work.
//
// An undecodable checkpoint contributes nothing rather than failing the call:
// the callers that build views report the decode error themselves, and the
// callers that select a removal must not be blocked by one corrupt record.
func FoldedEntryIDs(entries []SessionEntry) map[string]bool {
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
//
// Compaction is applied HERE, and only here. A checkpoint entry contributes no
// items of its own; instead it reshapes the view: every entry it folded
// (ExcludedIDs) is dropped wherever it appears, its Summary renders up front —
// it stands in for the oldest history — and each fold's stand-in renders where
// the folded group was. The entries a pass kept are projected from the list
// itself, never from a copy inside the checkpoint, so an entry popped after the
// pass is simply gone. An override for EntryKindCompaction disables all of this
// and is called per checkpoint like any other projector; the exclusions still
// apply, because they are facts about the view rather than a way of rendering
// it.
//
// Every checkpoint IN THE LIST is taken at its word. Callers pass a single
// branch's view (ContextEntries does); handing over append order across
// branches would let an abandoned attempt's checkpoint fold entries the
// active branch still reads.
func ProjectEntries(entries []SessionEntry, overrides map[EntryKind]EntryProjector) ([]InputItem, error) {
	folded := FoldedEntryIDs(entries)
	_, checkpointOverridden := overrides[EntryKindCompaction]

	// The render plan: summaries (and anchorless stand-ins) up front, anchored
	// stand-ins keyed by the entry they render before. Only live checkpoints
	// render — one that was itself folded is out of the view like anything
	// else, its exclusions already counted by FoldedEntryIDs.
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
				// A system message, not a user one: nobody said this. It is
				// context the runtime supplies in place of folded history.
				front = append(front, InputItemsFromSystemText(p.Summary)...)
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
					// The anchor is not in this view — a filtered read, or the
					// anchor entry was itself removed. Fronting the stand-in
					// keeps its content over losing its position.
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

// NewCompactionEntry builds a compaction checkpoint from what a pass folded
// away. The entries the pass KEPT are not part of it: they stay in the session
// and the projection reads them from there, so the checkpoint holds nothing it
// would have to keep in step with the tree.
func NewCompactionEntry(p CompactionPayload) (SessionEntry, error) {
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
