package store

import (
	"context"
	"testing"
)

// Own-over-global resolves to the caller's PRIVATE row. Owning the global one
// is not shadowing: the author published it, so their private row of the same
// name still wins for them (decisions §5.29).
func TestGetByNameForPrefersOwnPrivateOverOwnGlobal(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	skills := NewSkillStore(db)

	published := &Skill{Name: "helper", Description: "the team's", Content: "global body",
		Scope: ScopeGlobal, OwnerID: id("alice")}
	if err := skills.Create(ctx, published); err != nil {
		t.Fatal(err)
	}
	shadow := &Skill{Name: "helper", Description: "alice's own", Content: "private body",
		Scope: ScopePrivate, OwnerID: id("alice")}
	if err := skills.Create(ctx, shadow); err != nil {
		t.Fatalf("a private shadow of one's own published name must be legal: %v", err)
	}

	got, err := skills.GetByNameFor(ctx, "helper", id("alice"))
	if err != nil || got.ID != shadow.ID {
		t.Fatalf("author's own name resolves to %+v (%v), want their private shadow", got, err)
	}
	// Another member sees only the published one.
	got, err = skills.GetByNameFor(ctx, "helper", id("bob"))
	if err != nil || got.ID != published.ID {
		t.Fatalf("another member resolves to %+v (%v), want the published row", got, err)
	}
}

// The unique name indexes key on the repo LABEL, not the raw source URL: two
// sources that reduce to one label would otherwise both answer to one
// model-facing name and make read_skill a coin flip (decisions §5.31).
func TestSkillNameUniquePerRepoLabel(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	skills := NewSkillStore(db)

	repo := &Skill{Name: "pdf", Description: "d", Content: "c", Scope: ScopeGlobal, OwnerID: id("alice"),
		SourceRepo: "https://github.com/o/r"}
	if err := skills.Create(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if repo.RepoLabel != "o/r" || repo.QualifiedName() != "o/r:pdf" {
		t.Fatalf("label/qualified name = %q / %q", repo.RepoLabel, repo.QualifiedName())
	}
	// A blob URL of the same repository imports as a raw file but labels the same.
	blob := &Skill{Name: "pdf", Description: "d", Content: "c2", Scope: ScopeGlobal, OwnerID: id("alice"),
		SourceRepo: "https://github.com/o/r/blob/main/pdf/SKILL.md"}
	if _, dup := UniqueViolation(skills.Create(ctx, blob)); !dup {
		t.Fatal("a second source reducing to one label must violate the unique index")
	}
	// A different repo of the same name is a different label — and legal.
	other := &Skill{Name: "pdf", Description: "d", Content: "c3", Scope: ScopeGlobal, OwnerID: id("alice"),
		SourceRepo: "https://github.com/other/r"}
	if err := skills.Create(ctx, other); err != nil {
		t.Fatalf("a distinct repo sharing a skill name must be legal: %v", err)
	}
}
