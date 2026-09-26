package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// GetEntries is the whole session, oldest first.
func TestGetEntriesReturnsAllOldestFirst(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	s := NewEntryStoreFor(db, session.Direct(id("s1")))
	s.SetRunID(id("r1"))

	for i := range 5 {
		seed(t, s, userEntry(t, fmt.Sprint(i)))
	}

	all, err := s.GetEntries(ctx, session.Direct(id("s1")))
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("want 5 entries, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("entries not oldest-first at %d", i)
		}
	}
}

func TestTraceRetention(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ts := NewTraceStore(db)
	id := ids(t)

	old := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", Name: "old", CreatedAt: time.Now().UTC().AddDate(0, 0, -40)}
	recent := &TraceEvent{SessionID: id("s1"), RunID: id("r2"), Kind: "span", Name: "new", CreatedAt: time.Now().UTC()}
	if _, err := db.NewInsert().Model(old).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewInsert().Model(recent).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	n, err := ts.DeleteOlderThan(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 pruned, got %d", n)
	}
	left, _ := ts.ListBySession(ctx, id("s1"), "", 0)
	if len(left) != 1 || left[0].Name != "new" {
		t.Fatalf("wrong survivor: %+v", left)
	}
}
