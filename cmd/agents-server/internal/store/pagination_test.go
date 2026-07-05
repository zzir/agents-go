package store

import (
	"context"
	"testing"
	"time"
)

func TestGetMessagesPagination(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMessageStore(db)

	// Insert 5 item messages in order.
	for i := range 5 {
		m := NewItemMessageRaw("s1", "r1", "gpt", []byte(`{"role":"user","content":"m"}`))
		if _, err := db.NewInsert().Model(&m).Exec(ctx); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// No limit: all 5, oldest-first.
	all, err := ms.GetMessages(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("want 5 messages, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("messages not oldest-first at %d", i)
		}
	}

	// limit=2 returns the newest two, still ascending.
	page, err := ms.GetMessages(ctx, "s1", 0, 2)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[3].ID || page[1].ID != all[4].ID {
		t.Fatalf("newest page wrong: %+v", page)
	}

	// before_id cursor: everything older than the page's first id, newest 2.
	older, err := ms.GetMessages(ctx, "s1", page[0].ID, 2)
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

	old := &TraceEvent{SessionID: "s1", RunID: "r1", Kind: "span", Name: "old", CreatedAt: time.Now().UTC().AddDate(0, 0, -40)}
	recent := &TraceEvent{SessionID: "s1", RunID: "r2", Kind: "span", Name: "new", CreatedAt: time.Now().UTC()}
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
	left, _ := ts.ListBySession(ctx, "s1", 0, 0)
	if len(left) != 1 || left[0].Name != "new" {
		t.Fatalf("wrong survivor: %+v", left)
	}
}
