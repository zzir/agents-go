package store

import (
	"context"
	"strings"
	"testing"

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
	if p := resetCheckpoint(t, sa); !p.Reset {
		t.Fatal("the threshold did not reset")
	}

	sa2 := NewEntryStoreFor(db, session.Direct(NewID()))
	seedExchange(t, sa2)
	ca2 := NewCompactionAdapter(sa2, &summaryFakeModel{summary: "unused"}, 1_000_000, 2, "", CompactionNotifier{})
	if err := ca2.RunCompaction(ctx, session.CompactionArgs{Force: true, Reset: true}); err != nil {
		t.Fatal(err)
	}
	if p := resetCheckpoint(t, sa2); !p.Reset {
		t.Fatal("a Reset argument did not reset")
	}
}
