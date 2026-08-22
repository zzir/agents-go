package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

func TestGetEntriesPagination(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	s := NewEntryStoreFor(db, session.Direct(id("s1")))
	s.SetRunID(id("r1"))

	for i := range 5 {
		seed(t, s, userEntry(t, fmt.Sprint(i)))
	}

	// No limit: all 5, oldest-first.
	all, err := s.GetEntries(ctx, session.Direct(id("s1")), "", 0)
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

	// limit=2 returns the newest two, still ascending.
	page, err := s.GetEntries(ctx, session.Direct(id("s1")), "", 2)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[3].ID || page[1].ID != all[4].ID {
		t.Fatalf("newest page wrong: %+v", page)
	}

	// before_id cursor: everything older than the page's first id, newest 2.
	older, err := s.GetEntries(ctx, session.Direct(id("s1")), page[0].ID, 2)
	if err != nil {
		t.Fatalf("get older: %v", err)
	}
	if len(older) != 2 || older[1].ID != all[2].ID {
		t.Fatalf("cursor page wrong: %+v", older)
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
