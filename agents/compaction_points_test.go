package agents

import (
	"context"
	"errors"
	"testing"
)

// recordingCompactor drops the oldest n entries and records every pass.
type recordingCompactor struct {
	drop  int
	err   error
	calls [][]SessionEntry
}

func (c *recordingCompactor) Compact(_ context.Context, entries []SessionEntry) ([]SessionEntry, error) {
	c.calls = append(c.calls, entries)
	if c.err != nil {
		return nil, c.err
	}
	if len(entries) <= c.drop {
		return entries, nil
	}
	return entries[c.drop:], nil
}

func seededSession(t *testing.T, texts ...string) *Session {
	t.Helper()
	st := NewInMemoryStorage("test")
	items := make([]InputItem, 0, len(texts))
	for _, text := range texts {
		items = append(items, InputItemsFromText(text)...)
	}
	entries, err := NewItemEntries(items, Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), entries...); err != nil {
		t.Fatal(err)
	}
	return NewSession(st)
}

// The BeforeRun pass shapes what the very first model call sees — the point of
// it is that a long conversation does not blow its window on turn one.
func TestCompaction_BeforeRunShrinksTheFirstCall(t *testing.T) {
	c := &recordingCompactor{drop: 2}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one", "two", "three")},
		Compaction:   CompactionOptions{Compactor: c, Points: CompactBeforeRun},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 1 {
		t.Fatalf("compactor ran %d times, want 1", len(c.calls))
	}
	if len(c.calls[0]) != 3 {
		t.Errorf("compactor saw %d entries, want the 3 stored ones", len(c.calls[0]))
	}
	// 3 stored − 2 dropped + the new input.
	if got := len(model.lastReq.Input); got != 2 {
		t.Errorf("model input = %d items, want 2 (compacted history + new input)", got)
	}
}

// The SavePoint pass is the one the old design lacked: a run that keeps calling
// tools must be able to shrink its context without waiting for the run to end.
func TestCompaction_SavePointShrinksMidRun(t *testing.T) {
	c := &recordingCompactor{drop: 1}
	tool := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "result", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "old")},
		Compaction:   CompactionOptions{Compactor: c, Points: CompactAtSavePoint},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 1 {
		t.Fatalf("compactor ran %d times, want 1 (the single turn boundary)", len(c.calls))
	}
	// The save-point pass rebuilds the context from the log, so the second
	// model call must be shorter than the naive history it replaced.
	saw := len(c.calls[0])
	if saw < 4 {
		t.Errorf("compactor saw %d entries, want the persisted turn (old + input + call + output)", saw)
	}
	if got := len(model.lastReq.Input); got != saw-1 {
		t.Errorf("second call sent %d items, want %d (one entry dropped)", got, saw-1)
	}
}

// Points selects the moments; the zero value means all of them, so a caller who
// sets a Compactor and nothing else gets compaction everywhere.
func TestCompaction_PointsSelectTheMoments(t *testing.T) {
	run := func(t *testing.T, points CompactionPoint) int {
		t.Helper()
		c := &recordingCompactor{drop: 1}
		tool := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
			return "result", nil
		})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}
		agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
		if _, err := RunSync(context.Background(), agent, "go", RunOptions{
			Conversation: ConversationOptions{Session: seededSession(t, "old")},
			Compaction:   CompactionOptions{Compactor: c, Points: points},
		}); err != nil {
			t.Fatal(err)
		}
		return len(c.calls)
	}

	if n := run(t, CompactBeforeRun); n != 1 {
		t.Errorf("before-run only ran %d passes, want 1", n)
	}
	if n := run(t, CompactAtSavePoint); n != 1 {
		t.Errorf("save-point only ran %d passes, want 1", n)
	}
	if n := run(t, 0); n != 2 {
		t.Errorf("the zero value ran %d passes, want 2 (before run + the turn boundary)", n)
	}
}

// A compactor that fails must not fail the run: the context it was shrinking is
// still valid, so this is housekeeping that did not happen.
func TestCompaction_FailureIsBestEffort(t *testing.T) {
	c := &recordingCompactor{err: errors.New("summarizer down")}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one", "two")},
		Compaction:   CompactionOptions{Compactor: c},
	})
	if err != nil {
		t.Fatalf("a failed compaction pass failed the run: %v", err)
	}
	if res.FinalOutputString() != "ok" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// The uncompacted history still reached the model.
	if got := len(model.lastReq.Input); got != 3 {
		t.Errorf("model input = %d items, want the 3 uncompacted ones", got)
	}
}

// Compaction never has to reason about server-held history: the two are
// refused at the door, so a run whose history the server keeps has no local
// entries for a compactor to see.
func TestCompaction_ServerHeldHistoryExcludesALocalSession(t *testing.T) {
	agent := &Agent{Name: "a", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}}
	for name, conv := range map[string]ConversationOptions{
		"previous response id": {Session: seededSession(t, "one"), UsePreviousResponseID: true},
		"conversation id":      {Session: seededSession(t, "one"), ConversationID: "conv_1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := RunSync(context.Background(), agent, "now", RunOptions{
				Conversation: conv,
				Compaction:   CompactionOptions{Compactor: &recordingCompactor{drop: 1}},
			})
			var ue *UserError
			if !errors.As(err, &ue) {
				t.Errorf("err = %v, want a *UserError", err)
			}
		})
	}
}

func TestCompactionPoint_Has(t *testing.T) {
	both := CompactBeforeRun | CompactAfterRun
	if !both.Has(CompactBeforeRun) || !both.Has(CompactAfterRun) {
		t.Error("a combined point does not report its members")
	}
	if both.Has(CompactAtSavePoint) {
		t.Error("a combined point reports a member it does not have")
	}
}

// checkpointingCompactor drops the oldest entry and reports a checkpoint for it.
type checkpointingCompactor struct {
	dropped []string
}

func (c *checkpointingCompactor) Compact(_ context.Context, entries []SessionEntry) ([]SessionEntry, error) {
	if len(entries) < 2 {
		return entries, nil
	}
	c.dropped = append(c.dropped, entries[0].ID)
	return entries[1:], nil
}

func (c *checkpointingCompactor) Checkpoint(_ []SessionEntry) (SessionEntry, bool, error) {
	if len(c.dropped) == 0 {
		return SessionEntry{}, false, nil
	}
	e, err := NewCompactionEntry(CompactionPayload{
		Summary:     "earlier discussion",
		ExcludedIDs: c.dropped,
	})
	return e, err == nil, err
}

// The after-run checkpoint is what makes compaction persist: the next run reads
// from it instead of recomputing the same pass.
func TestCompaction_AfterRunWritesACheckpoint(t *testing.T) {
	ctx := context.Background()
	sess := seededSession(t, "one", "two", "three")
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	if _, err := RunSync(ctx, agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: sess},
		Compaction:   CompactionOptions{Compactor: &checkpointingCompactor{}, Points: CompactAfterRun},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint *SessionEntry
	for i := range all {
		if all[i].Kind == EntryKindCompaction {
			checkpoint = &all[i]
		}
	}
	if checkpoint == nil {
		t.Fatal("no checkpoint was appended")
	}
	p, err := checkpoint.CompactionPayload()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ExcludedIDs) == 0 {
		t.Error("the checkpoint names nothing as excluded")
	}

	// Nothing was rewritten: every folded entry is still in the log, which is
	// what lets a reader expand them and a fork keep its full history.
	byID := map[string]bool{}
	for _, e := range all {
		byID[e.ID] = true
	}
	for _, id := range p.ExcludedIDs {
		if !byID[id] {
			t.Errorf("entry %q was excluded AND removed; compaction must only append", id)
		}
	}

	// The next run's view drops what the checkpoint folded and keeps the
	// checkpoint, whose summary the projection renders in the folded
	// history's place.
	ctxEntries, err := sess.ContextEntries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	sawCheckpoint := false
	for _, e := range ctxEntries {
		if e.Kind == EntryKindCompaction {
			sawCheckpoint = true
		}
		for _, id := range p.ExcludedIDs {
			if e.ID == id {
				t.Errorf("folded entry %q is still in the context view", id)
			}
		}
	}
	if !sawCheckpoint {
		t.Error("the checkpoint is not part of the context view")
	}
}

// A compactor that replaces N entries with N different ones has compacted
// something. Deciding "no change" from the count alone discards that pass
// silently — the save point would rebuild nothing and the after-run
// checkpoint would never be written.
func TestCompactionDetectsSameLengthRewrite(t *testing.T) {
	before := []SessionEntry{
		{ID: "e1", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"the long original"}`)},
		{ID: "e2", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"and another"}`)},
	}

	same := []SessionEntry{
		{ID: "e1", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"the long original"}`)},
		{ID: "e2", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"and another"}`)},
	}
	if changedEntries(before, same) {
		t.Error("identical entries reported as changed; every turn would rebuild")
	}

	// Same count, different content: a summary standing in for an entry.
	rewritten := []SessionEntry{
		{ID: "c1", Kind: EntryKindItem, Item: []byte(`{"role":"system","content":"[summary] the long original"}`)},
		{ID: "e2", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"and another"}`)},
	}
	if !changedEntries(before, rewritten) {
		t.Error("a same-length rewrite was reported as no change; the pass would be discarded")
	}

	// Same ids, different content — the case an id-only check would miss.
	edited := []SessionEntry{
		{ID: "e1", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"shortened"}`)},
		{ID: "e2", Kind: EntryKindItem, Item: []byte(`{"role":"user","content":"and another"}`)},
	}
	if !changedEntries(before, edited) {
		t.Error("an in-place edit was reported as no change")
	}
}
