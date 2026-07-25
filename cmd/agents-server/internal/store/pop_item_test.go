package store

import (
	"context"
	"encoding/json"
	"testing"
)

// PopItem must honor the Session contract: (nil, nil) on an empty session, and
// it must only ever pop a replayable item — never a UI-only annotation or a
// compacted (soft-deleted) row.
func TestSessionAdapterPopItem(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	a := NewSessionAdapter(db, sid)

	// Empty session -> (nil, nil), not an error.
	got, err := a.PopEntry(ctx)
	if err != nil || got != nil {
		t.Fatalf("empty session: got=%v err=%v, want nil,nil", got, err)
	}

	insert := func(m Message) {
		if _, err := db.NewInsert().Model(&m).Exec(ctx); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	userRaw, _ := json.Marshal(map[string]any{"role": "user", "content": "hi"})
	// Oldest -> newest: a real item, then a compacted item, then an annotation.
	insert(NewItemMessageRaw(sid, "r", "m", userRaw))
	compacted := NewItemMessageRaw(sid, "r", "m", userRaw)
	compacted.Compacted = true
	insert(compacted)
	insert(NewAnnotationMessage(sid, "r", "error", "boom"))

	// The newest ROW is the annotation and the one before is compacted, but
	// PopItem must skip both and return the real item.
	got, err = a.PopEntry(ctx)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if got == nil {
		t.Fatal("expected the replayable item, got nil")
	}

	// The annotation and compacted rows must still be present (not deleted).
	var remaining []Message
	if err := db.NewSelect().Model(&remaining).Where("session_id = ?", sid).Scan(ctx); err != nil {
		t.Fatalf("scan remaining: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining rows = %d, want 2 (annotation + compacted untouched)", len(remaining))
	}
	for _, m := range remaining {
		if m.Kind != MessageKindAnnotation && !m.Compacted {
			t.Errorf("PopItem deleted the wrong row; a plain item survived: %+v", m)
		}
	}

	// No replayable items left -> (nil, nil) again even though rows exist.
	got, err = a.PopEntry(ctx)
	if err != nil || got != nil {
		t.Fatalf("no replayable items: got=%v err=%v, want nil,nil", got, err)
	}
}

// The DB enforces provider-route prefix uniqueness, and the violation is
// classified for a 409 by UniqueViolation.
func TestProviderRoutePrefixUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewProviderRouteStore(db)
	if err := s.Create(ctx, &ProviderRoute{ID: NewID(), Prefix: "gpt", APIKey: "k"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.Create(ctx, &ProviderRoute{ID: NewID(), Prefix: "gpt", APIKey: "k2"})
	if err == nil {
		t.Fatal("duplicate prefix must violate the unique index")
	}
	if cols, ok := UniqueViolation(err); !ok || cols != "prefix" {
		t.Errorf("UniqueViolation = %q,%v want \"prefix\",true", cols, ok)
	}
}
