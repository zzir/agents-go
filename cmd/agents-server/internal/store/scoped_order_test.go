package store

import (
	"context"
	"testing"
	"time"
)

// Every scoped listing uses one order: the shared rows first — what a member
// picks from — then each group newest first, so what somebody just made is
// where they look for it (spec §5.29).
func TestScopedListingIsGlobalFirstNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	agents := NewAgentConfigStore(db)

	base := time.Now().UTC().Add(-time.Hour)
	for i, row := range []struct {
		name  string
		scope string
		owner string
	}{
		{"global-old", ScopeGlobal, id("alice")},
		{"mine-old", ScopePrivate, id("alice")},
		{"global-new", ScopeGlobal, id("bob")},
		{"mine-new", ScopePrivate, id("alice")},
		{"theirs", ScopePrivate, id("bob")},
	} {
		ac := &AgentConfig{ID: NewID(), Name: row.name, Model: "m", Scope: row.scope, OwnerID: row.owner,
			CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := agents.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListVisibleOf(ctx, agents.CrudStore, id("alice"), false)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, ac := range got {
		names = append(names, ac.Name)
	}
	want := []string{"global-new", "global-old", "mine-new", "mine-old"} // bob's private row is not alice's to see
	if len(names) != len(want) {
		t.Fatalf("listing = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("listing = %v, want %v", names, want)
		}
	}
}
