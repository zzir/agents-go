package store

import (
	"context"
	"strings"
	"testing"
)

// One document's failed write is that document's skip, never the import's:
// each lands in its own savepoint, so the rows after a refused one are still
// created. On PostgreSQL a failed INSERT in the outer transaction would
// otherwise abort every statement after it (25P02).
func TestApplyImportSkipsOneDocAndLandsTheRest(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	skills := NewSkillStore(db)

	const repo = "https://github.com/o/r"
	// A row of the same group already holds the name "pdf" at another path,
	// so a fresh "pdf" collides on the private-scope name index.
	taken := &Skill{Name: "pdf", Description: "d", Content: "old", Scope: ScopePrivate, OwnerID: id("alice"),
		SourceRepo: repo, SourcePath: "docs/pdf/SKILL.md"}
	if err := skills.Create(ctx, taken); err != nil {
		t.Fatal(err)
	}
	docs := []ImportDoc{
		{Path: "pdf/SKILL.md", SHA: "1", Name: "pdf", Description: "d", Content: "new"},
		{Path: "other/SKILL.md", SHA: "2", Name: "other", Description: "d", Content: "other"},
	}
	out, err := skills.ApplyImport(ctx, repo, id("alice"), ScopePrivate, true, docs)
	if err != nil {
		t.Fatalf("ApplyImport: %v", err)
	}
	if len(out) != 2 || out[0].Action != "skipped" || !strings.Contains(out[0].Reason, "already in use") {
		t.Fatalf("outcomes = %+v, want the collision skipped by name", out)
	}
	if out[1].Action != "created" {
		t.Fatalf("outcomes = %+v, want the second document created after the skip", out)
	}
	rows, err := skills.ListMeta(ctx, id("alice"), false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if len(rows) != 2 {
		t.Fatalf("skills after import = %v, want pdf and other", names)
	}
}
