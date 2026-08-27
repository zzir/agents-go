package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/uptrace/bun"
)

// Create runs behind locks on the target AND the template rows: a missing
// either refuses the insert instead of leaving a project row that points at
// nothing (decisions §5.28).
func TestProjectCreateRequiresTargetAndTemplate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	projects := NewProjectStore(db)
	tpl := createTemplateRow(t, db)

	p := &Project{ID: NewID(), OwnerID: LocalUserID, TargetID: NewID(), TemplateID: tpl, Name: "p"}
	if err := projects.Create(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create on a missing target: err=%v, want ErrNotFound", err)
	}

	createTargetRow(t, db, id("tg-1"))
	p = &Project{ID: NewID(), OwnerID: LocalUserID, TargetID: id("tg-1"), TemplateID: NewID(), Name: "p"}
	if err := projects.Create(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create on a missing template: err=%v, want ErrNotFound", err)
	}

	p = &Project{ID: NewID(), OwnerID: LocalUserID, TargetID: id("tg-1"), TemplateID: tpl, Name: "p"}
	if err := projects.Create(ctx, p); err != nil {
		t.Fatalf("create on existing rows: %v", err)
	}
}

// createTemplateRow persists a docker template and returns its id.
func createTemplateRow(t *testing.T, db *bun.DB) string {
	t.Helper()
	tpl := &SandboxTemplate{ID: NewID(), Name: "tpl-" + NewID(), Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := NewSandboxTemplateStore(db).Create(context.Background(), tpl); err != nil {
		t.Fatalf("create sandbox template: %v", err)
	}
	return tpl.ID
}

// List scopes to one owner — EveryOwner is the admin listing across all —
// and each row carries its bound-session count.
func TestProjectListScopeAndSessionCount(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	projects := NewProjectStore(db)

	createTargetRow(t, db, id("tg"))
	tpl := createTemplateRow(t, db)
	mine := &Project{ID: NewID(), OwnerID: LocalUserID, TargetID: id("tg"), TemplateID: tpl, Name: "mine"}
	foreign := &Project{ID: NewID(), OwnerID: NewID(), TargetID: id("tg"), TemplateID: tpl, Name: "foreign"}
	for _, p := range []*Project{mine, foreign} {
		if err := projects.Create(ctx, p); err != nil {
			t.Fatalf("create project %s: %v", p.Name, err)
		}
	}
	s := &Session{ID: NewID(), OwnerID: LocalUserID, Name: "s", ProjectID: mine.ID}
	if err := NewSessionStore(db).Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	own, err := projects.List(ctx, LocalUserID)
	if err != nil {
		t.Fatalf("list own: %v", err)
	}
	if len(own) != 1 || own[0].ID != mine.ID {
		t.Fatalf("own listing = %+v, want just %s", own, mine.ID)
	}
	if own[0].SessionCount != 1 {
		t.Fatalf("session count = %d, want 1", own[0].SessionCount)
	}

	all, err := projects.List(ctx, EveryOwner)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("EveryOwner listing has %d rows, want 2", len(all))
	}
}

// Create builds its own transaction (it locks the target and template rows), so it bypasses
// the CrudStore write path that seals — the seal has to be applied there too,
// or the one path that CREATES an environment writes it in the clear.
func TestProjectEnvSealedOnCreate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	withTestBox(t)
	projects := NewProjectStore(db)
	createTargetRow(t, db, id("tg"))
	tpl := createTemplateRow(t, db)

	env, err := NormalizeProjectEnv([]EnvVar{{Key: "TOKEN", Value: "sk-live"}, {Key: "TZ", Value: "UTC"}})
	if err != nil {
		t.Fatal(err)
	}
	p := &Project{ID: NewID(), OwnerID: LocalUserID, TargetID: id("tg"), TemplateID: tpl, Name: "p", Env: env}
	if err := projects.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.Env != env {
		t.Errorf("caller's env after Create = %q, want plaintext", p.Env)
	}
	// Every value, not a chosen few: the environment is write-only
	// (decisions §5.32).
	raw := rawColumn(t, db, "SELECT env FROM projects WHERE id = ?", p.ID)
	if strings.Contains(raw, "sk-live") || strings.Contains(raw, "UTC") {
		t.Errorf("env at rest = %q, want every value sealed", raw)
	}
	got, err := projects.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env != env {
		t.Errorf("Get env = %q, want the plaintext %q", got.Env, env)
	}
	if got.Revision != 1 || got.RuntimeGen != 1 {
		t.Errorf("counters after create = %d/%d, want 1/1", got.Revision, got.RuntimeGen)
	}
}

// The update is a compare-and-set, and only a CONTENT change moves the
// runtime generation — a rename must not replace anyone's container.
func TestProjectUpdateCASAndGenerations(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	withTestBox(t)
	projects := NewProjectStore(db)
	createTargetRow(t, db, id("tg"))
	tpl := createTemplateRow(t, db)

	p := &Project{ID: NewID(), OwnerID: LocalUserID, TargetID: id("tg"), TemplateID: tpl, Name: "p"}
	if err := projects.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	renamed := *p
	renamed.Name = "renamed"
	if err := projects.Update(ctx, p.ID, &renamed, 1, false); err != nil {
		t.Fatal(err)
	}
	got, err := projects.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.Revision != 2 || got.RuntimeGen != 1 {
		t.Errorf("after rename: name=%s rev=%d gen=%d, want renamed/2/1", got.Name, got.Revision, got.RuntimeGen)
	}

	env, err := NormalizeProjectEnv([]EnvVar{{Key: "TOKEN", Value: "sk-live"}})
	if err != nil {
		t.Fatal(err)
	}
	withEnv := *got
	withEnv.Env = env
	if err := projects.Update(ctx, p.ID, &withEnv, 2, true); err != nil {
		t.Fatal(err)
	}
	if got, err = projects.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got.Env != env || got.Revision != 3 || got.RuntimeGen != 2 {
		t.Errorf("after env change: rev=%d gen=%d env=%q, want 3/2/%q", got.Revision, got.RuntimeGen, got.Env, env)
	}
	if raw := rawColumn(t, db, "SELECT env FROM projects WHERE id = ?", p.ID); strings.Contains(raw, "sk-live") {
		t.Errorf("env at rest after update = %q, want sealed", raw)
	}

	cleared := *got
	cleared.Env = ""
	if err := projects.Update(ctx, p.ID, &cleared, 3, true); err != nil {
		t.Fatal(err)
	}
	if got, err = projects.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got.Env != "" || got.RuntimeGen != 3 {
		t.Errorf("after clearing: env=%q gen=%d, want \"\"/3", got.Env, got.RuntimeGen)
	}

	stale := *got
	stale.Name = "loser"
	if err := projects.Update(ctx, p.ID, &stale, 3, false); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: err=%v, want ErrRevisionConflict", err)
	}
	// Owner and target are identity, not editable content.
	moved := *got
	moved.OwnerID, moved.TargetID = NewID(), NewID()
	if err := projects.Update(ctx, p.ID, &moved, 4, false); err != nil {
		t.Fatal(err)
	}
	if got, err = projects.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got.OwnerID != LocalUserID || got.TargetID != id("tg") {
		t.Errorf("owner/target after update = %s/%s, want them unchanged", got.OwnerID, got.TargetID)
	}
	if err := projects.Update(ctx, NewID(), &stale, 1, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing project: err=%v, want ErrNotFound", err)
	}
}
