package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func seedExchange(t *testing.T, sa *EntryStore) {
	t.Helper()
	seed(t, sa,
		userEntry(t, "first question"),
		assistantEntry(t, "first answer"),
		rawEntryFrom(t, `{"type":"function_call","call_id":"c1","name":"read_file","arguments":"{}"}`, agents.Source{Type: agents.SourceModel}),
		toolOutputEntry(t, "c1", "package main"),
		userEntry(t, "second question"),
		assistantEntry(t, "second answer"),
	)
}

func resetCheckpoint(t *testing.T, sa *EntryStore) session.CompactionPayload {
	t.Helper()
	entries, err := sa.load(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if last.Kind != session.EntryKindCompaction {
		t.Fatalf("last entry is a %s, want the checkpoint", last.Kind)
	}
	p, err := last.CompactionPayload()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A reset keeps the newest user message, folds every other item on the
// branch, and carries the session memory in the checkpoint; the model then
// reads the summary and that message, nothing else.
func TestResetPassKeepsTheNewestUserMessage(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	seedExchange(t, sa)
	memories := NewMemoryStore(db)
	if err := memories.Upsert(ctx, mem(MemoryScopeSession, sessionID, "", "notes.md", "the plan: fix the parser"), nil); err != nil {
		t.Fatal(err)
	}
	var before, after int
	ca := NewCompactionAdapter(sa, nil, 1_000_000, 2, "", CompactionNotifier{OnDone: func(b, a int) { before, after = b, a }})
	ca.Mode, ca.Memories = CompactionModeReset, memories

	if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		t.Fatal(err)
	}
	if before != 6 || after != 2 {
		t.Fatalf("OnDone(%d, %d), want (6, 2)", before, after)
	}
	p := resetCheckpoint(t, sa)
	if !p.Reset || len(p.ExcludedIDs) != 5 {
		t.Fatalf("checkpoint = %+v", p)
	}
	for _, want := range []string{"The context was reset.", "the plan: fix the parser", "history_search"} {
		if !strings.Contains(p.Summary, want) {
			t.Fatalf("summary lacks %q: %q", want, p.Summary)
		}
	}
	items, err := session.NewSession(sa).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, session.ItemText(it))
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "the plan: fix the parser") || texts[1] != "second question" {
		t.Fatalf("model view = %q", texts)
	}
	// The folded turn stays on the branch for the transcript: the checkpoint
	// extends the tip as it stood, folded or not (invariant 24).
	view, err := sa.GetEntries(ctx, session.Direct(sessionID), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range view {
		if !e.OnPath {
			t.Fatalf("entry %s (compacted=%v) fell off the branch after the reset", e.ID, e.Compacted)
		}
	}
}

// Hybrid carries a recap from the summary model in front of the memory; a
// model that fails leaves a bare reset rather than no reset.
func TestResetPassHybridRecap(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewEntryStoreFor(db, session.Direct(NewID()))
	seedExchange(t, sa)
	model := &summaryFakeModel{summary: "They looked at the parser and found the bug."}
	ca := NewCompactionAdapter(sa, model, 1_000_000, 2, "", CompactionNotifier{})
	ca.Mode = CompactionModeHybrid
	if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		t.Fatal(err)
	}
	if p := resetCheckpoint(t, sa); !p.Reset || !strings.Contains(p.Summary, "found the bug") || strings.Contains(p.Summary, "The context was reset.") {
		t.Fatalf("hybrid checkpoint = %q", p.Summary)
	}
	if model.calls != 1 {
		t.Fatalf("recap calls = %d", model.calls)
	}

	sa2 := NewEntryStoreFor(db, session.Direct(NewID()))
	seedExchange(t, sa2)
	failing := &summaryFakeModel{summary: ""}
	ca2 := NewCompactionAdapter(sa2, failing, 1_000_000, 2, "", CompactionNotifier{})
	ca2.Mode = CompactionModeHybrid
	if err := ca2.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		t.Fatal(err)
	}
	if p := resetCheckpoint(t, sa2); !p.Reset || !strings.Contains(p.Summary, "The context was reset.") {
		t.Fatalf("a recap that came back empty still resets: %q", p.Summary)
	}
}

// The threshold trips the same pass in reset mode, and a Reset argument
// resets even an agent in summary mode.
func TestResetPassTriggers(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewEntryStoreFor(db, session.Direct(NewID()))
	seedExchange(t, sa)
	ca := NewCompactionAdapter(sa, nil, 1, 2, "", CompactionNotifier{})
	ca.Mode = CompactionModeReset
	if err := ca.RunCompaction(ctx, session.CompactionArgs{}); err != nil {
		t.Fatal(err)
	}
	if p := resetCheckpoint(t, sa); !p.Reset || !strings.Contains(p.Summary, "past the agent's threshold") {
		t.Fatalf("the threshold did not reset as the threshold's: %q", p.Summary)
	}

	sa2 := NewEntryStoreFor(db, session.Direct(NewID()))
	seedExchange(t, sa2)
	ca2 := NewCompactionAdapter(sa2, &summaryFakeModel{summary: "unused"}, 1_000_000, 2, "", CompactionNotifier{})
	if err := ca2.RunCompaction(ctx, session.CompactionArgs{Force: true, Reset: true}); err != nil {
		t.Fatal(err)
	}
	if p := resetCheckpoint(t, sa2); !p.Reset || !strings.Contains(p.Summary, "without calling new_context again") {
		t.Fatalf("a Reset argument did not reset as the model's: %q", p.Summary)
	}
}

// A second reset folds the first reset's checkpoint too: the model reads one
// summary, the newest snapshot, never one per reset.
func TestResetPassFoldsEarlierCheckpoints(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	memories := NewMemoryStore(db)
	ca := NewCompactionAdapter(sa, nil, 1_000_000, 2, "", CompactionNotifier{})
	ca.Mode, ca.Memories = CompactionModeReset, memories

	seed(t, sa, userEntry(t, "first question"), assistantEntry(t, "first answer"))
	if err := memories.Upsert(ctx, mem(MemoryScopeSession, sessionID, "", "notes.md", "snapshot one"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		t.Fatal(err)
	}
	first := resetCheckpoint(t, sa)

	seed(t, sa, userEntry(t, "second question"), assistantEntry(t, "second answer"))
	if err := memories.Upsert(ctx, mem(MemoryScopeSession, sessionID, "", "notes.md", "snapshot two"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		t.Fatal(err)
	}

	items, err := session.NewSession(sa).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, session.ItemText(it))
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "snapshot two") || strings.Contains(texts[0], "snapshot one") || texts[1] != "second question" {
		t.Fatalf("model view after two resets = %q", texts)
	}
	all, err := sa.load(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints, compactedCheckpoints int
	for _, e := range all {
		if e.Kind == session.EntryKindCompaction {
			checkpoints++
			p, _ := e.CompactionPayload()
			if p.Summary == first.Summary {
				row := new(entryRow)
				if err := db.NewSelect().Model(row).Where("entry_id = ?", e.ID).Scan(ctx); err != nil {
					t.Fatal(err)
				}
				if row.Compacted {
					compactedCheckpoints++
				}
			}
		}
	}
	if checkpoints != 2 || compactedCheckpoints != 1 {
		t.Fatalf("checkpoints = %d, earlier one folded = %d", checkpoints, compactedCheckpoints)
	}
}

// Appends that race hold the row: none of them overwrites another's text.
func TestMemoryAppendsNeverLoseText(t *testing.T) {
	appendRace(t, newTestDB(t))
}

func appendRace(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	s := NewMemoryStore(db)
	sc := MemoryScope{Kind: MemoryScopeSession, ID: NewID(), Gen: "g"}
	const writers, each = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := s.AppendContent(ctx, sc, "log.md", fmt.Sprintf("[w%d-%d]", w, i), MemoryWrittenByModel, "", nil); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append: %v", err)
	}
	got, err := s.GetByKey(ctx, sc, "log.md")
	if err != nil {
		t.Fatal(err)
	}
	for w := range writers {
		for i := range each {
			if !strings.Contains(got.Content, fmt.Sprintf("[w%d-%d]", w, i)) {
				t.Fatalf("append [w%d-%d] was lost; content = %q", w, i, got.Content)
			}
		}
	}
}
