package store

import (
	"context"
	"testing"
	"time"
)

// A trigger's delivery is owed once per key — the replay guard, held by the
// table — while a task's debts are not held to it. What a claim took can be
// reopened, and a delivery forgotten may be owed again.
func TestTriggerDeliveryIsOwedOnce(t *testing.T) {
	ctx := context.Background()
	s := NewWakeupStore(newTestDB(t))
	id := ids(t)
	delivery := func(trigger, key string) *Wakeup {
		return &Wakeup{SessionID: id("s"), Kind: WakeKindTrigger, SourceID: id(trigger), Attempt: key}
	}
	first := delivery("trg", "sig-1")
	for i, w := range []*Wakeup{first, delivery("trg", "sig-1")} {
		if created, err := s.Owe(ctx, w); err != nil || created != (i == 0) {
			t.Fatalf("Owe #%d = %v, %v; want the first recorded and the second a replay", i, created, err)
		}
	}
	// Another key, another trigger, and a task under the same attempt are not replays.
	for name, w := range map[string]*Wakeup{
		"another key":     delivery("trg", "sig-2"),
		"another trigger": delivery("other", "sig-1"),
		"a task's debt":   {SessionID: id("s"), Kind: WakeKindTask, SourceID: id("trg"), Attempt: "sig-1"},
	} {
		if created, err := s.Owe(ctx, w); err != nil || !created {
			t.Fatalf("%s: Owe = %v, %v; want it recorded", name, created, err)
		}
	}
	owed, err := s.PendingFor(ctx, WakeKindTrigger, id("trg"))
	if err != nil || len(owed) != 2 || owed[0].Attempt != "sig-1" || owed[1].Attempt != "sig-2" {
		t.Fatalf("backlog = %+v (%v), want the trigger's two, oldest first", owed, err)
	}

	// A claim takes it out of the backlog; reopened, it waits again.
	if won, err := s.Settle(ctx, first.ID, first.Attempt, WakeDelivered); err != nil || !won {
		t.Fatalf("claim = %v, %v", won, err)
	}
	if owed, _ := s.PendingFor(ctx, WakeKindTrigger, id("trg")); len(owed) != 1 {
		t.Fatalf("backlog after a claim = %d, want 1", len(owed))
	}
	if err := s.Reopen(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if owed, _ := s.PendingFor(ctx, WakeKindTrigger, id("trg")); len(owed) != 2 {
		t.Fatalf("backlog after reopening = %d, want 2", len(owed))
	}
	// Still held, it is a replay; forgotten, it is not.
	if created, _ := s.Owe(ctx, delivery("trg", "sig-1")); created {
		t.Fatal("a delivery still waiting was recorded twice")
	}
	if err := s.Forget(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if created, err := s.Owe(ctx, delivery("trg", "sig-1")); err != nil || !created {
		t.Fatalf("Owe after Forget = %v, %v; want it recorded again", created, err)
	}
}

// Settled wake-ups older than the cutoff are pruned; a pending one of any age
// is not — it is still owed.
func TestDeleteSettledBefore(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewWakeupStore(db)
	id := ids(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	rows := []*Wakeup{
		{ID: id("w-delivered"), SessionID: id("s"), Kind: "task", State: WakeDelivered, CreatedAt: old},
		{ID: id("w-cancelled"), SessionID: id("s"), Kind: "task", State: WakeCancelled, CreatedAt: old},
		{ID: id("w-pending"), SessionID: id("s"), Kind: "task", State: WakePending, CreatedAt: old},
		{ID: id("w-fresh"), SessionID: id("s"), Kind: "task", State: WakeDelivered, CreatedAt: time.Now().UTC()},
	}
	for _, w := range rows {
		if _, err := db.NewInsert().Model(w).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteSettledBefore(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2", n, err)
	}
	var left []Wakeup
	if err := db.NewSelect().Model(&left).OrderExpr("id").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, w := range left {
		got[w.ID] = true
	}
	if len(left) != 2 || !got[id("w-fresh")] || !got[id("w-pending")] {
		t.Fatalf("remaining = %+v", left)
	}
}
