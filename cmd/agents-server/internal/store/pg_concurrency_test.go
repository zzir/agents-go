package store

import (
	"context"
	"errors"
	"fmt"
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

// A first-run bind and a project delete race on the project row's lock (FOR
// KEY SHARE vs FOR UPDATE): whichever commits first, no interleaving may end
// with a session bound to a deleted project — there is no unbind path. On
// READ COMMITTED, the old single-statement guards let both commit.
func TestPGBindVsProjectDeleteNeverDangles(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	id := ids(t)
	createSandboxRow(t, db, id("sb"))
	projects := NewProjectStore(db)
	sessions := NewSessionStore(db)

	for i := range 20 {
		p := &Project{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("sb"), Name: fmt.Sprintf("p-%d", i)}
		if err := projects.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		sess := &Session{ID: NewID(), OwnerID: LocalUserID, Name: "s"}
		if err := sessions.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var bound bool
		var bindErr, delErr error
		wg.Go(func() { bound, bindErr = sessions.BindProjectIfEmpty(ctx, sess.ID, p.ID) })
		wg.Go(func() { _, delErr = projects.DeleteIfUnreferenced(ctx, p.ID) })
		wg.Wait()
		if bindErr != nil || delErr != nil {
			t.Fatalf("round %d: bind=%v delete=%v", i, bindErr, delErr)
		}
		_, gerr := projects.Get(ctx, p.ID)
		gone := gerr != nil
		if gone && !errors.Is(gerr, ErrNotFound) {
			t.Fatal(gerr)
		}
		got, err := sessions.Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if bound && gone {
			t.Fatalf("round %d: the bind won and the delete still removed the project", i)
		}
		if got.ProjectID != "" && gone {
			t.Fatalf("round %d: session bound to the deleted project %s", i, got.ProjectID)
		}
	}
}

// A project create and a sandbox delete race on the sandbox row's FOR UPDATE
// lock: either the create lands first and the delete refuses, or the delete
// wins and the create refuses (ErrNotFound) — never a project on a deleted
// sandbox.
func TestPGProjectCreateVsSandboxDeleteNeverOrphans(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	projects := NewProjectStore(db)
	sandboxes := NewSandboxStore(db)

	for i := range 20 {
		sbID := NewID()
		createSandboxRow(t, db, sbID)
		p := &Project{ID: NewID(), OwnerID: LocalUserID, SandboxID: sbID, Name: fmt.Sprintf("p-%d", i)}
		var wg sync.WaitGroup
		var createErr, delErr error
		wg.Go(func() { createErr = projects.Create(ctx, p) })
		wg.Go(func() { _, delErr = sandboxes.DeleteIfUnreferenced(ctx, sbID) })
		wg.Wait()
		if createErr != nil && !errors.Is(createErr, ErrNotFound) {
			t.Fatalf("round %d: create: %v", i, createErr)
		}
		if delErr != nil {
			t.Fatalf("round %d: delete: %v", i, delErr)
		}
		_, perr := projects.Get(ctx, p.ID)
		if perr != nil && !errors.Is(perr, ErrNotFound) {
			t.Fatal(perr)
		}
		sbExists, serr := db.NewSelect().Model((*Sandbox)(nil)).Where("id = ?", sbID).Exists(ctx)
		if serr != nil {
			t.Fatal(serr)
		}
		if perr == nil && !sbExists {
			t.Fatalf("round %d: project survived on a deleted sandbox", i)
		}
	}
}
