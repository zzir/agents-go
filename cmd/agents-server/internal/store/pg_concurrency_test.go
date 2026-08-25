package store

import (
	"context"
	"sync"
	"testing"
)

// Every agent write takes the PROVIDER lock before the agent's — an update
// and a transfer racing over one pair must not each hold what the other
// waits for. On PostgreSQL a reversed order aborts one transaction with a
// deadlock; SQLite's single writer cannot show it, so this test needs a real
// server (AGENTS_PG_TEST_DSN).
func TestPGAgentWritesShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	id := ids(t)
	seedUsers(t, db, id("alice"), id("bob"))
	providers := NewProviderStore(db)
	pv := &Provider{ID: id("pv"), Name: "shared", Type: "openai", Scope: ScopeGlobal, OwnerID: id("alice")}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	agents := NewAgentConfigStore(db)
	ac := &AgentConfig{ID: NewID(), Name: "a", Model: "m", ProviderID: pv.ID, Scope: ScopePrivate, OwnerID: id("alice")}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}

	// Updates and transfers, interleaved: a lock-order inversion surfaces as a
	// deadlock error on one of them.
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m := *ac
			m.Model = "m2"
			errs <- agents.Update(ctx, ac.ID, &m, nil)
		}()
		owner := id("alice")
		if i%2 == 0 {
			owner = id("bob")
		}
		go func() {
			defer wg.Done()
			errs <- agents.TransferOwner(ctx, ac.ID, owner)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent agent writes: %v", err)
		}
	}
}
