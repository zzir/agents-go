package store

import (
	"context"
	"testing"
	"time"
)

// The log pages backwards on the event id — a UUIDv7, so it orders like
// created_at and never ties: rows sharing a timestamp across a page boundary
// are neither skipped nor repeated. The page size is clamped, not reset.
func TestAuditListRecentPagesOnID(t *testing.T) {
	ctx := context.Background()
	s := NewAuditStore(newTestDB(t))
	for i := range 7 {
		ev := &AuditEvent{ActorID: LocalUserID, ActorEmail: "local@localhost", Action: "POST /agents", Resource: string(rune('a' + i))}
		if err := s.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// One timestamp for all, so only the id can order them.
	if _, err := s.db.NewUpdate().Model((*AuditEvent)(nil)).Set("created_at = ?", time.Unix(1_700_000_000, 0).UTC()).Where("1 = 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	var seen []string
	before := ""
	for {
		page, err := s.ListRecent(ctx, 3, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			seen = append(seen, ev.Resource)
		}
		before = page[len(page)-1].ID
	}
	if got := len(seen); got != 7 {
		t.Fatalf("paged %d events %v, want all 7 once each", got, seen)
	}
	if seen[0] != "g" || seen[6] != "a" {
		t.Fatalf("order = %v, want newest first", seen)
	}
	big, err := s.ListRecent(ctx, 9999, "")
	if err != nil || len(big) != 7 {
		t.Fatalf("an oversized limit must be clamped, not reset: %d, %v", len(big), err)
	}
}

// A prune deletes in batches and counts what it removed.
func TestAuditDeleteOlderThanBatches(t *testing.T) {
	ctx := context.Background()
	s := NewAuditStore(newTestDB(t))
	defer func(n int) { pruneBatchSize = n }(pruneBatchSize)
	pruneBatchSize = 5
	for range 12 {
		if err := s.Record(ctx, &AuditEvent{ActorID: LocalUserID, Action: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.NewUpdate().Model((*AuditEvent)(nil)).Set("created_at = ?", time.Now().Add(-48*time.Hour)).Where("1 = 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, &AuditEvent{ActorID: LocalUserID, Action: "fresh"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteOlderThan(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 12 {
		t.Fatalf("pruned %d, %v; want 12", n, err)
	}
	left, _ := s.ListRecent(ctx, 0, "")
	if len(left) != 1 || left[0].Action != "fresh" {
		t.Fatalf("left = %+v, want the fresh one", left)
	}
}
