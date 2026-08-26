package store

import (
	"context"
	"errors"
	"testing"
)

// A repo group is one owner as it is one scope: transferring into an owner who
// already holds a group for the repo would merge two groups whose scopes the
// unique indexes cannot compare, so it is refused (decisions §5.31).
func TestSetRepoOwnerRefusesAMerge(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	seedUsers(t, db, id("alice"), id("bob"))
	skills := NewSkillStore(db)

	const repo = "https://github.com/o/r"
	alice := &Skill{Name: "pdf", Description: "d", Content: "c", Scope: ScopeGlobal, OwnerID: id("alice"), SourceRepo: repo}
	bob := &Skill{Name: "pdf", Description: "d", Content: "c", Scope: ScopePrivate, OwnerID: id("bob"), SourceRepo: repo}
	for _, sk := range []*Skill{alice, bob} {
		if err := skills.Create(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	if err := skills.SetRepoOwner(ctx, repo, id("alice"), id("bob")); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("transfer into an existing group = %v, want ErrGroupExists", err)
	}
	if got, _ := skills.Get(ctx, alice.ID); got.OwnerID != id("alice") {
		t.Fatalf("a refused transfer moved the row: owner = %s", got.OwnerID)
	}
	// With no group in the way it goes through, whole.
	if err := skills.SetRepoOwner(ctx, repo, id("bob"), id("carol")); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("transfer to an unknown account = %v, want ErrNoSuchUser", err)
	}
}

// An import names the group it refreshes: the lookups are owner-EXACT, so an
// admin who holds their own copy of a repo cannot refresh it while asking for
// somebody else's published group (decisions §5.31).
func TestFindBySourceAndRepoGroupAreOwnerExact(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	skills := NewSkillStore(db)

	const repo = "https://github.com/o/r"
	published := &Skill{Name: "pdf", Description: "d", Content: "theirs", Scope: ScopeGlobal, OwnerID: id("alice"),
		SourceRepo: repo, SourcePath: "pdf/SKILL.md"}
	mine := &Skill{Name: "pdf", Description: "d", Content: "mine", Scope: ScopePrivate, OwnerID: id("admin"),
		SourceRepo: repo, SourcePath: "pdf/SKILL.md"}
	for _, sk := range []*Skill{published, mine} {
		if err := skills.Create(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	got, err := skills.FindBySource(ctx, repo, "pdf/SKILL.md", id("alice"))
	if err != nil || got == nil || got.ID != published.ID {
		t.Fatalf("FindBySource(alice) = %+v (%v), want the published row", got, err)
	}
	scope, ok, err := skills.RepoGroup(ctx, repo, id("alice"))
	if err != nil || !ok || scope != ScopeGlobal {
		t.Fatalf("RepoGroup(alice) = (%s, %v, %v), want the published group", scope, ok, err)
	}
	scope, ok, _ = skills.RepoGroup(ctx, repo, id("admin"))
	if !ok || scope != ScopePrivate {
		t.Fatalf("RepoGroup(admin) = (%s, %v), want their own private group", scope, ok)
	}
	if got, _ := skills.FindBySource(ctx, repo, "pdf/SKILL.md", id("bob")); got != nil {
		t.Fatalf("FindBySource for an owner with no group = %+v, want nil", got)
	}
}

// A transfer re-checks the leg that spends a credential AS THE NEW OWNER: an
// agent handed to somebody who cannot see its provider would answer 204 and
// then fail every run (decisions §5.29).
func TestAgentTransferChecksProviderVisibility(t *testing.T) {
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
	ac := &AgentConfig{ID: NewID(), Name: "a", Model: "m", ProviderID: pv.ID, Scope: ScopePrivate, OwnerID: id("alice")}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	if err := agents.TransferOwner(ctx, ac.ID, id("bob")); !errors.Is(err, ErrProviderScope) {
		t.Fatalf("transfer onto a provider bob cannot see = %v, want ErrProviderScope", err)
	}
	if got, _ := agents.Get(ctx, ac.ID); got.OwnerID != id("alice") {
		t.Fatalf("a refused transfer moved the row: owner = %s", got.OwnerID)
	}
	// Publishing the provider makes it visible to everyone, and the transfer lands.
	if err := SetScopeOf(ctx, providers.CrudStore, pv.ID, ScopeGlobal, id("alice")); err != nil {
		t.Fatal(err)
	}
	if err := agents.TransferOwner(ctx, ac.ID, id("bob")); err != nil {
		t.Fatalf("transfer onto a global provider: %v", err)
	}
	if got, _ := agents.Get(ctx, ac.ID); got.OwnerID != id("bob") {
		t.Fatalf("transferred agent owner = %s, want bob", got.OwnerID)
	}
}
