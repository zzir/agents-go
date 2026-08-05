package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Projection decides what the model reads. Recording something and sending it
// are different acts, and the session used to have no way to say so.
func TestProjectEntries_OnlyContextKindsReachTheModel(t *testing.T) {
	item, err := NewItemEntry(InputItemsFromText("real question")[0], Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	entries := []SessionEntry{
		item,
		NewAnnotationEntry(ItemDisplay{Kind: DisplayMessage, Text: "run cancelled"}, Source{}),
		{Kind: EntryKindTerminal, Payload: json.RawMessage(`{"output":"ls -la"}`)},
		{Kind: EntryKindCustom, CustomType: "sticky_note", Payload: json.RawMessage(`{}`)},
		{Kind: "a_kind_from_the_future", Payload: json.RawMessage(`{}`)},
	}

	got, err := ProjectEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("projected %d items, want only the conversation item", len(got))
	}
	raw, _ := MarshalInputItem(got[0])
	if !strings.Contains(string(raw), "real question") {
		t.Errorf("projected the wrong entry: %s", raw)
	}
}

// A caller can opt a kind into context — the documented use is showing the
// model what was run by hand in a terminal.
func TestProjectEntries_CallerOverridesTheDefaults(t *testing.T) {
	entries := []SessionEntry{
		{Kind: EntryKindTerminal, Payload: json.RawMessage(`{"command":"go test ./..."}`)},
	}
	got, err := ProjectEntries(entries, map[EntryKind]EntryProjector{
		EntryKindTerminal: func(e SessionEntry) ([]TResponseInputItem, error) {
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, err
			}
			return InputItemsFromText("$ " + p.Command), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("projected %d items, want 1", len(got))
	}
	raw, _ := MarshalInputItem(got[0])
	if !strings.Contains(string(raw), "go test") {
		t.Errorf("override did not apply: %s", raw)
	}

	// And a projector mapped to nil suppresses a kind that would otherwise
	// reach the model.
	item, _ := NewItemEntry(InputItemsFromText("hi")[0], Source{})
	got, err = ProjectEntries([]SessionEntry{item}, map[EntryKind]EntryProjector{EntryKindItem: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a nil projector should suppress the kind, got %d items", len(got))
	}
}

// An update amends a display without rewriting the entry it targets, which is
// what keeps the log append-only.
func TestFoldUpdates(t *testing.T) {
	target := SessionEntry{
		ID:      "e1",
		Kind:    EntryKindItem,
		Display: &ItemDisplay{Kind: DisplayToolCall, ToolName: "spawn_task", Text: "running"},
	}
	first, err := NewUpdateEntry("e1", ItemDisplay{Text: "still running", Extra: map[string]any{"pct": 40}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewUpdateEntry("e1", ItemDisplay{Text: "done", Extra: map[string]any{"pct": 100}})
	if err != nil {
		t.Fatal(err)
	}

	got := FoldUpdates([]SessionEntry{target, first, second})
	if len(got) != 1 {
		t.Fatalf("folded to %d entries, want 1 (updates are consumed)", len(got))
	}
	d := got[0].Display
	if d.Text != "done" {
		t.Errorf("Text = %q, want the last update to win", d.Text)
	}
	if d.ToolName != "spawn_task" {
		t.Errorf("ToolName = %q, want the target's untouched field preserved", d.ToolName)
	}
	// float64: an update payload round-trips through JSON, like every stored
	// entry does.
	if d.Extra["pct"] != float64(100) {
		t.Errorf("Extra.pct = %v (%T), want 100", d.Extra["pct"], d.Extra["pct"])
	}
}

// THE reason updates are entries: an update may be stored before its target.
// A background task can finish before the turn that spawned it is persisted,
// and projection associates them by id regardless of order — which removes the
// race rather than retrying around it.
func TestFoldUpdates_UpdateMayPrecedeItsTarget(t *testing.T) {
	update, err := NewUpdateEntry("e1", ItemDisplay{Text: "finished first"})
	if err != nil {
		t.Fatal(err)
	}
	target := SessionEntry{ID: "e1", Kind: EntryKindItem, Display: &ItemDisplay{Kind: DisplayToolCall}}

	got := FoldUpdates([]SessionEntry{update, target})
	if len(got) != 1 || got[0].Display.Text != "finished first" {
		t.Fatalf("an update stored before its target did not apply: %+v", got)
	}
}

// Folding is a projection: it must never edit what storage holds. The stored
// entry's Display (and its Extra map) is shared with what readers get back —
// storage hands out shallow copies — so the merge has to copy, not write
// through. This used to fail exactly when the target already had Extra and an
// update brought more.
func TestFoldUpdates_DoesNotWriteThroughToStorage(t *testing.T) {
	st := NewInMemoryStorage("s")
	target := SessionEntry{
		ID:      "e1",
		Kind:    EntryKindItem,
		Display: &ItemDisplay{Kind: DisplayToolCall, CallID: "c1", Extra: map[string]any{"pct": float64(40)}},
	}
	update, err := NewUpdateEntry("e1", ItemDisplay{Extra: map[string]any{"pct": float64(100), "done": true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), target, update); err != nil {
		t.Fatal(err)
	}

	read, err := st.Entries(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	folded := FoldUpdates(read)
	if len(folded) != 1 || folded[0].Display.Extra["pct"] != float64(100) {
		t.Fatalf("fold did not apply: %+v", folded)
	}

	again, err := st.Entries(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if got := again[0].Display.Extra; len(got) != 1 || got["pct"] != float64(40) {
		t.Errorf("a read-side fold modified storage: Extra = %v, want the original {pct: 40}", got)
	}
}

// An update whose target is gone is ignored, not an error: compaction may have
// folded the target away, and failing a whole read over a stale pointer would
// make history unloadable.
func TestFoldUpdates_MissingTargetIsIgnored(t *testing.T) {
	update, err := NewUpdateEntry("nobody", ItemDisplay{Text: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	target := SessionEntry{ID: "e1", Kind: EntryKindItem}
	got := FoldUpdates([]SessionEntry{target, update})
	if len(got) != 1 || got[0].ID != "e1" {
		t.Fatalf("folded = %+v, want just the surviving entry", got)
	}
}

// Updates never reach the model — they amend a display, and a display is not
// something anyone said.
func TestProjectEntries_UpdatesAreNotContext(t *testing.T) {
	update, err := NewUpdateEntry("e1", ItemDisplay{Text: "amended"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ProjectEntries([]SessionEntry{update}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an update reached the model: %v", got)
	}
}

// A run's entries carry provenance and display, so a reader gets the timeline
// the run produced instead of re-deriving it from the wire item.
func TestRunPersistsEntriesWithProvenanceAndDisplay(t *testing.T) {
	session := NewInMemorySession()
	tool := NewFunctionTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "tool out", nil })
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "t", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{
		Conversation: ConversationOptions{Session: NewSession(session)},
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := session.Entries(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 4 {
		t.Fatalf("stored %d entries, want the input plus the turn's items", len(entries))
	}

	var sawUser, sawToolOutput bool
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("entry stored without an id: %+v", e)
		}
		if e.CreatedAt.IsZero() {
			t.Errorf("entry stored without a timestamp: %+v", e)
		}
		switch e.Source.Type {
		case SourceUser:
			sawUser = true
		case SourceTool:
			sawToolOutput = true
			if e.Display == nil || e.Display.Output != "tool out" {
				t.Errorf("tool output entry lost its display: %+v", e.Display)
			}
		}
	}
	if !sawUser {
		t.Error("no entry attributed to the user")
	}
	if !sawToolOutput {
		t.Error("no entry attributed to a tool")
	}
}

// A compaction checkpoint copies nothing: it names what it folded, its summary
// renders up front, and the kept history projects from the entries themselves.
func TestCompactionCheckpointProjection(t *testing.T) {
	folded, err := NewItemEntry(InputItemsFromText("earlier discussion detail")[0], Source{})
	if err != nil {
		t.Fatal(err)
	}
	folded.ID = "e1"
	kept, err := NewItemEntry(InputItemsFromText("the most recent question")[0], Source{})
	if err != nil {
		t.Fatal(err)
	}
	kept.ID = "e2"
	cp, err := NewCompactionEntry(CompactionPayload{
		Summary:     "SUMMARY: earlier discussion",
		ExcludedIDs: []string{folded.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	cp.ID = "e3"

	got, err := ProjectEntries([]SessionEntry{folded, kept, cp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("projected %d items, want the summary plus the kept entry", len(got))
	}
	raw, _ := MarshalInputItem(got[0])
	// A system message: nobody said this, it is context the runtime supplied.
	if !strings.Contains(string(raw), `"system"`) || !strings.Contains(string(raw), "SUMMARY") {
		t.Errorf("the summary is not a system message rendered first: %s", raw)
	}
	if strings.Contains(string(raw), "earlier discussion detail") {
		t.Errorf("folded history was re-sent: %s", raw)
	}
	raw, _ = MarshalInputItem(got[1])
	if !strings.Contains(string(raw), "most recent question") {
		t.Errorf("the kept entry was lost: %s", raw)
	}
}

// An amender that knows the tool call but not the entry can still amend it —
// which is the case for anything reporting on a call afterwards, since the
// entry id is assigned by storage at a moment the amender may not have reached.
func TestUpdateEntry_TargetsACallID(t *testing.T) {
	call, err := EntryFromRunItem(&ToolCallItem{
		Raw: functionCallOutput(t, "spawn_task", "call-9", `{}`),
	}, "resp_1")
	if err != nil {
		t.Fatal(err)
	}
	call.ID = "e2"

	upd, err := NewCallUpdateEntry("call-9", ItemDisplay{
		Extra: map[string]any{"task_status": "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The update is stored BEFORE its target — the case the retry loop used to
	// exist for — and folding still associates them.
	folded := FoldUpdates([]SessionEntry{upd, call})
	if len(folded) != 1 {
		t.Fatalf("folded to %d entries, want the update merged away", len(folded))
	}
	got := folded[0].Display
	if got == nil || got.Extra["task_status"] != "completed" {
		t.Errorf("display = %+v, want the update applied", got)
	}
	// The call's own fields survive the merge.
	if got.ToolName != "spawn_task" || got.CallID != "call-9" {
		t.Errorf("display lost the call's own fields: %+v", got)
	}
}

// An update naming a call nothing holds amends nothing, rather than attaching
// itself to whatever happened to be nearby.
func TestUpdateEntry_UnknownCallIDIsIgnored(t *testing.T) {
	call, err := EntryFromRunItem(&ToolCallItem{
		Raw: functionCallOutput(t, "probe", "call-1", `{}`),
	}, "r")
	if err != nil {
		t.Fatal(err)
	}
	call.ID = "e1"
	upd, err := NewCallUpdateEntry("call-does-not-exist", ItemDisplay{Text: "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	folded := FoldUpdates([]SessionEntry{call, upd})
	if len(folded) != 1 || (folded[0].Display != nil && folded[0].Display.Text == "ghost") {
		t.Errorf("an unmatched update was applied: %+v", folded)
	}
}
