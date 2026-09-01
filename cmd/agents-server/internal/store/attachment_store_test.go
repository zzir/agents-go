package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func TestAttachmentStoreLifecycle(t *testing.T) {
	db := testdb.New(t)
	s := store.NewAttachmentStore(db)
	ctx := context.Background()

	a := &store.Attachment{OwnerID: store.LocalUserID, Key: "att/a.png", Mime: "image/png", Size: 10}
	if err := s.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("Create did not mint an id")
	}

	got, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "att/a.png" || got.Bound {
		t.Fatalf("got %+v", got)
	}

	// MetaBatch: present ids resolve, absent ids are simply missing.
	meta, err := s.MetaBatch(ctx, []string{a.ID, store.NewID()})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != 1 || meta[a.ID].Key != "att/a.png" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta, err = s.MetaBatch(ctx, nil); err != nil || len(meta) != 0 {
		t.Fatalf("empty batch: %v %v", meta, err)
	}

	// MarkBound is idempotent.
	for range 2 {
		if err := s.MarkBound(ctx, []string{a.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ = s.Get(ctx, a.ID); !got.Bound {
		t.Fatal("not bound after MarkBound")
	}

	// A bound row is never an orphan; an old unbound one is.
	orphan := &store.Attachment{OwnerID: store.LocalUserID, Key: "att/o.png", Mime: "image/png", Size: 1}
	if err := s.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	old, err := s.ListUnboundBefore(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || old[0].ID != orphan.ID {
		t.Fatalf("orphans = %+v", old)
	}
	if old, _ = s.ListUnboundBefore(ctx, time.Now().UTC().Add(-time.Minute)); len(old) != 0 {
		t.Fatalf("future-created row listed as orphan: %+v", old)
	}

	// Delete is idempotent — the reaper may retry a half-done removal.
	if err := s.Delete(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, orphan.ID); err == nil {
		t.Fatal("deleted row still answers")
	}
}

func TestAttachmentSentinel(t *testing.T) {
	id := "0123"
	if got := store.AttachmentSentinelID(store.AttachmentSentinelURL(id)); got != id {
		t.Fatalf("round trip = %q", got)
	}
	if store.AttachmentSentinelID("https://example.com/x.png") != "" {
		t.Fatal("plain URL must not parse as sentinel")
	}
}
