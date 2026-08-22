package store

import (
	"context"
	"testing"
	"time"
)

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
