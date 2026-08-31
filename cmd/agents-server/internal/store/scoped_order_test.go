package store

import (
	"context"
	"testing"
	"time"
)

// The four owner-grouped scoped entities order by AUTHORSHIP, not scope: a
// member sees others' shared rows first, then their own, each group newest
// first; an admin sees the whole table newest first, ungrouped. The group key
// is owner_id (permanent), so a scope flip never reorders a row (decisions
// §5.29).
func TestScopedListingOrder(t *testing.T) {
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
		{"mine-global-old", ScopeGlobal, id("alice")}, // t0 — alice published
		{"mine-private", ScopePrivate, id("alice")},   // t1
		{"theirs-global-mid", ScopeGlobal, id("bob")}, // t2 — bob shared
		{"mine-global-new", ScopeGlobal, id("alice")}, // t3 — alice published
		{"theirs-global-new", ScopeGlobal, id("bob")}, // t4 — bob shared
		{"theirs-private", ScopePrivate, id("bob")},   // t5 — not alice's to see
	} {
		ac := &AgentConfig{ID: NewID(), Name: row.name, Model: "m", Scope: row.scope, OwnerID: row.owner,
			CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := agents.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
	}

	// Member (alice): others first (newest first), then alice's own (newest
	// first, scope ignored) — so her published globals sink to her own group.
	// bob's private row is not hers to see.
	assertOrder(t, agents, id("alice"), false,
		"theirs-global-new", "theirs-global-mid",
		"mine-global-new", "mine-private", "mine-global-old")

	// Admin: the whole table, newest first, ungrouped — bob's private included.
	assertOrder(t, agents, id("alice"), true,
		"theirs-private", "theirs-global-new", "mine-global-new",
		"theirs-global-mid", "mine-private", "mine-global-old")
}

func assertOrder(t *testing.T, s *AgentConfigStore, viewer string, admin bool, want ...string) {
	t.Helper()
	got, err := ListVisibleOf(context.Background(), s.CrudStore, viewer, admin)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, ac := range got {
		names = append(names, ac.Name)
	}
	if len(names) != len(want) {
		t.Fatalf("listing = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("listing = %v, want %v", names, want)
		}
	}
}
