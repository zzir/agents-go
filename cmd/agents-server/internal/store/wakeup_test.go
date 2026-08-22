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
	old := time.Now().UTC().Add(-48 * time.Hour)
	rows := []*Wakeup{
		{ID: "w-delivered", SessionID: "s", Kind: "task", State: WakeDelivered, CreatedAt: old},
		{ID: "w-cancelled", SessionID: "s", Kind: "task", State: WakeCancelled, CreatedAt: old},
		{ID: "w-pending", SessionID: "s", Kind: "task", State: WakePending, CreatedAt: old},
		{ID: "w-fresh", SessionID: "s", Kind: "task", State: WakeDelivered, CreatedAt: time.Now().UTC()},
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
	if len(left) != 2 || left[0].ID != "w-fresh" || left[1].ID != "w-pending" {
		t.Fatalf("remaining = %+v", left)
	}
}
