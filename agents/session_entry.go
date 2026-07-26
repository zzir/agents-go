package agents

import (
	"encoding/json"
	"fmt"
	"time"
)

// EntryKind classifies what a session entry holds. It is an open vocabulary:
// a consumer that meets a kind it does not know must ignore the entry, not
// fail, so a session written by a newer build stays readable.
type EntryKind string

const (
	// EntryKindItem is a Responses item — the conversation itself.
	EntryKindItem EntryKind = "item"
	// EntryKindAnnotation is for people, not the model: an error banner, the
	// partial output left behind by a cancelled run.
	EntryKindAnnotation EntryKind = "annotation"
	// EntryKindCompaction is a compaction checkpoint: a summary plus the tail
	// it retained.
	EntryKindCompaction EntryKind = "compaction"
	// EntryKindTerminal is output from an interactive terminal session.
	EntryKindTerminal EntryKind = "terminal"
	// EntryKindUpdate amends an earlier entry's display. See UpdatePayload.
	EntryKindUpdate EntryKind = "update"
	// EntryKindLeaf moves the session's active branch. See LeafPayload.
	EntryKindLeaf EntryKind = "leaf"
	// EntryKindCustom is the extension point; CustomType names the subtype.
	EntryKindCustom EntryKind = "custom"
)

// SessionEntry is one record in a session's history.
//
// Sessions used to store bare Responses items, which meant everything that was
// not a Responses item had nowhere to live: an error banner, a compaction
// checkpoint, terminal output. Consumers grew side tables for them and had to
// merge two orderings back together at read time.
//
// Entries are **append-only**. Nothing is ever rewritten in place — an entry
// whose display must change later gets an EntryKindUpdate naming it, folded in
// at projection time. That is what lets a session be shared, forked, and read
// concurrently without a writer invalidating a reader's view.
type SessionEntry struct {
	// ID identifies the entry within its session. Storage assigns it when
	// empty.
	ID string `json:"id"`
	// Seq is the entry's position in append order, assigned by storage. It is
	// what a Cursor pages on: an offset shifts under a concurrent append, a
	// sequence number does not.
	Seq int64 `json:"seq,omitzero"`
	// ParentID is the entry this one follows. Empty means a root.
	//
	// A session is a tree, not a list, because branching is the natural shape
	// of "try that again differently": the abandoned attempt stays recorded
	// instead of being deleted, and two branches can share everything before
	// the point they diverge rather than duplicating it.
	ParentID string `json:"parent_id,omitzero"`
	// Kind says what this entry holds.
	Kind EntryKind `json:"kind"`
	// CustomType names the subtype when Kind is EntryKindCustom.
	CustomType string `json:"custom_type,omitzero"`
	// Source records who produced the entry.
	Source Source `json:"source,omitzero"`
	// AgentName is the agent that produced it, when one did.
	AgentName string `json:"agent_name,omitzero"`
	// ResponseID ties the entry to the model call that produced it. Several
	// entries from one response share it, which is what makes per-response
	// usage attributable.
	ResponseID string `json:"response_id,omitzero"`

	// Item is the Responses item's wire JSON, for Kind == EntryKindItem.
	//
	// It is stored as raw bytes rather than a decoded union so an item type
	// this build does not model survives verbatim; see UnknownOutputItem.
	Item json.RawMessage `json:"item,omitzero"`
	// Payload is the structured body of every other kind.
	Payload json.RawMessage `json:"payload,omitzero"`

	// Display is the entry's UI projection, when it has one.
	Display *ItemDisplay `json:"display,omitzero"`
	// Usage is the token usage of the model call this entry belongs to — a call
	// on THIS conversation. Exactly one entry per response carries it, so
	// summing over entries counts each request once.
	Usage *RequestUsage `json:"usage,omitzero"`
	// Diagnostics records trouble the run went through while producing this
	// entry — retries, a fallback model, a compaction pass that gave up.
	//
	// It is on the entry rather than only in a log because these are the
	// failures that do NOT fail the run: they never reach an error return, so
	// without this the session cannot answer "why was that answer bad" once the
	// log has rotated.
	Diagnostics []Diagnostic `json:"diagnostics,omitzero"`

	// NestedUsage is what a nested run started by this entry's tool spent.
	//
	// Separate from Usage because they answer different questions. Usage
	// measures this conversation, and a reader estimating how large it has
	// grown reads the most recent one; a nested run's tokens were spent on a
	// different conversation, and counting them as context would make it look
	// larger than anything ever sent.
	NestedUsage *RequestUsage `json:"nested_usage,omitzero"`

	// CreatedAt is when the entry was produced. Storage sets it when zero.
	CreatedAt time.Time `json:"created_at"`
}

// UpdatePayload is the body of an EntryKindUpdate entry: it amends the display
// of an entry recorded earlier.
//
// It exists because some displays are settled long after the turn that produced
// them ended — a background task card whose task runs for minutes while its
// parent turn finished in seconds. Rewriting the original entry would break the
// append-only guarantee that forking and concurrent reads depend on.
//
// It also makes a race disappear rather than handling it. An update may be
// stored BEFORE its target, and projection still associates them by id; the
// consumer-side retry loop that existed to paper over "the task finished before
// the parent turn was saved" has nothing left to do.
type UpdatePayload struct {
	// TargetID is the entry being amended.
	TargetID string `json:"target_id"`
	// Display is merged over the target's display. Only non-zero fields apply.
	Display ItemDisplay `json:"display"`
}

// NewItemEntry builds an entry holding a Responses item.
func NewItemEntry(item TResponseInputItem, src Source) (SessionEntry, error) {
	raw, err := MarshalInputItem(item)
	if err != nil {
		return SessionEntry{}, fmt.Errorf("encoding session item: %w", err)
	}
	return SessionEntry{Kind: EntryKindItem, Source: src, Item: raw}, nil
}

// NewItemEntries builds item entries for a slice of input items.
func NewItemEntries(items []TResponseInputItem, src Source) ([]SessionEntry, error) {
	out := make([]SessionEntry, 0, len(items))
	for i, item := range items {
		e, err := NewItemEntry(item, src)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// EntryFromRunItem builds a session entry from a run item, carrying its
// provenance, display and owning agent.
func EntryFromRunItem(it RunItem, responseID string) (SessionEntry, error) {
	in, err := it.ToInputItem()
	if err != nil {
		return SessionEntry{}, err
	}
	e, err := NewItemEntry(in, it.Source())
	if err != nil {
		return SessionEntry{}, err
	}
	if a := it.AgentRef(); a != nil {
		e.AgentName = a.Name
	}
	d := it.Display()
	e.Display = &d
	e.ResponseID = responseID
	if out, ok := it.(*ToolCallOutputItem); ok && out.NestedUsage != nil {
		u := out.NestedUsage.Request()
		e.NestedUsage = &u
	}
	return e, nil
}

// NewAnnotationEntry builds an entry that is shown to people but never sent to
// the model: an error banner, a cancellation notice, partial output.
func NewAnnotationEntry(display ItemDisplay, src Source) SessionEntry {
	return SessionEntry{Kind: EntryKindAnnotation, Source: src, Display: &display}
}

// LeafPayload is the body of an EntryKindLeaf entry: it moves the session's
// active branch to another entry.
//
// Switching branches is an APPEND, not a mutable pointer. That is what keeps
// the switch itself part of the history — you can see that a branch was
// abandoned and when — and what lets the current leaf be derived by folding the
// log rather than stored beside it, where it could disagree after a crash.
type LeafPayload struct {
	// TargetID is the entry that becomes the new leaf.
	TargetID string `json:"target_id"`
}

// NewLeafEntry builds an entry moving the active branch to targetID.
func NewLeafEntry(targetID string) (SessionEntry, error) {
	raw, err := json.Marshal(LeafPayload{TargetID: targetID})
	if err != nil {
		return SessionEntry{}, fmt.Errorf("encoding leaf payload: %w", err)
	}
	return SessionEntry{Kind: EntryKindLeaf, Payload: raw}, nil
}

// LeafPayload decodes a leaf entry's payload.
func (e SessionEntry) LeafPayload() (LeafPayload, error) {
	if e.Kind != EntryKindLeaf {
		return LeafPayload{}, fmt.Errorf("entry %q is a %s entry, not a leaf move", e.ID, e.Kind)
	}
	var p LeafPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return LeafPayload{}, fmt.Errorf("decoding leaf payload: %w", err)
	}
	return p, nil
}

// NewUpdateEntry builds an entry amending an earlier entry's display.
func NewUpdateEntry(targetID string, display ItemDisplay) (SessionEntry, error) {
	raw, err := json.Marshal(UpdatePayload{TargetID: targetID, Display: display})
	if err != nil {
		return SessionEntry{}, fmt.Errorf("encoding update payload: %w", err)
	}
	return SessionEntry{Kind: EntryKindUpdate, Payload: raw}, nil
}

// InputItem decodes an item entry's Responses item.
func (e SessionEntry) InputItem() (TResponseInputItem, error) {
	if e.Kind != EntryKindItem {
		return TResponseInputItem{}, fmt.Errorf("entry %q is a %s entry, not an item", e.ID, e.Kind)
	}
	return UnmarshalInputItem(e.Item)
}

// UpdatePayload decodes an update entry's payload.
func (e SessionEntry) UpdatePayload() (UpdatePayload, error) {
	if e.Kind != EntryKindUpdate {
		return UpdatePayload{}, fmt.Errorf("entry %q is a %s entry, not an update", e.ID, e.Kind)
	}
	var p UpdatePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return UpdatePayload{}, fmt.Errorf("decoding update payload: %w", err)
	}
	return p, nil
}

// merge overlays a non-zero field of other onto d.
func (d *ItemDisplay) merge(other ItemDisplay) {
	if other.Kind != "" {
		d.Kind = other.Kind
	}
	if other.Renderer != "" {
		d.Renderer = other.Renderer
	}
	if other.Text != "" {
		d.Text = other.Text
	}
	if other.CallID != "" {
		d.CallID = other.CallID
	}
	if other.ToolName != "" {
		d.ToolName = other.ToolName
	}
	if other.Arguments != "" {
		d.Arguments = other.Arguments
	}
	if other.Output != "" {
		d.Output = other.Output
	}
	if other.IsError {
		d.IsError = true
	}
	if len(other.Extra) > 0 {
		if d.Extra == nil {
			d.Extra = make(map[string]any, len(other.Extra))
		}
		for k, v := range other.Extra {
			d.Extra[k] = v
		}
	}
}
