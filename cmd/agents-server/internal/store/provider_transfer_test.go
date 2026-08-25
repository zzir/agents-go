package store

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"
)

// A transfer moves the credential, so it carries the demote's guard: handing a
// PRIVATE provider to somebody else while agents still reference it would
// strand them at run time, so it is refused instead (spec §5.29).
func TestTransferOwnerGuardsReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	seedUsers(t, db, id("alice"), id("bob"))
	providers := NewProviderStore(db)
	pv := &Provider{ID: id("pv"), Name: "key", Type: "openai", Scope: ScopePrivate, OwnerID: id("alice")}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	agents := NewAgentConfigStore(db)
	ref := &AgentConfig{ID: NewID(), Name: "a", Model: "m", ProviderID: pv.ID, Scope: ScopePrivate, OwnerID: id("alice")}
	if err := agents.Create(ctx, ref); err != nil {
		t.Fatal(err)
	}

	refs, err := providers.TransferOwner(ctx, pv.ID, id("bob"))
	if err != nil || refs != 1 {
		t.Fatalf("transfer with a stranded agent = (%d, %v), want (1, nil)", refs, err)
	}
	if got, _ := providers.Get(ctx, pv.ID); got.OwnerID != id("alice") {
		t.Fatalf("a refused transfer moved the row: owner = %s", got.OwnerID)
	}

	if err := agents.Delete(ctx, ref.ID); err != nil {
		t.Fatal(err)
	}
	if refs, err = providers.TransferOwner(ctx, pv.ID, id("bob")); err != nil || refs != 0 {
		t.Fatalf("unblocked transfer = (%d, %v), want (0, nil)", refs, err)
	}
	got, err := providers.Get(ctx, pv.ID)
	if err != nil || got.OwnerID != id("bob") || got.Scope != ScopePrivate {
		t.Fatalf("transferred row = (%s, %s), want bob's private set", got.Scope, got.OwnerID)
	}
	if _, err := providers.TransferOwner(ctx, pv.ID, NewID()); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("transfer to an unknown account = %v, want ErrNoSuchUser", err)
	}
}

// seedUsers inserts the accounts a transfer's existence check reads.
func seedUsers(t *testing.T, db *bun.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := db.NewInsert().Model(&User{ID: id, Email: id + "@example.com", Role: RoleMember}).Exec(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
