package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// A sidecar that cannot be read must stop a delete, not wave it through. "team
// a" and "team+a" share the filename team_a, and the ID inside the file is the
// only thing that says which of them owns it. Failing open there means one
// corrupt file is enough for a delete of the id that does NOT exist to destroy
// the history of the one that does.
func TestDeleteRefusesWhenTheSidecarCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	repo := newRepo(t, dir)
	ctx := context.Background()

	if _, err := repo.Create(ctx, session.CreateOptions{ID: "team a"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Corrupt the sidecar in place: the file is still there and still owned by
	// "team a", but nothing can tell that from its contents any more.
	path := filepath.Join(dir, sanitizeSessionID("team a")+".meta.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := repo.Delete(ctx, "team+a"); err == nil {
		t.Fatal("delete through a colliding name succeeded against an unreadable sidecar")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the other session's metadata was removed anyway: %v", err)
	}
}

// A delete of a session that was never created is not an error: there is
// nothing to protect, and callers rely on it being idempotent.
func TestDeleteOfAMissingSessionSucceeds(t *testing.T) {
	repo := newRepo(t, t.TempDir())
	if err := repo.Delete(context.Background(), "never-existed"); err != nil {
		t.Fatalf("delete of a missing session: %v", err)
	}
}

// Create claims the sidecar with O_EXCL before it can write it, and gives the
// claim back if the write or the close then fails — otherwise the id is burned:
// List skips the half-written file, Open cannot parse it, and Create reports
// "already exists" forever.
//
// The rollback must fire ONLY for a claim this call made. A Create that failed
// BECAUSE the sidecar was already there is the dangerous case: rolling that
// back would delete a live session's metadata as a side effect of someone
// asking for a name that was taken.
func TestCreateDoesNotRollBackSomebodyElsesSidecar(t *testing.T) {
	dir := t.TempDir()
	repo := newRepo(t, dir)
	ctx := context.Background()

	if _, err := repo.Create(ctx, session.CreateOptions{ID: "taken", Title: "the original"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Same id, and a colliding one: both fail at the O_EXCL open, before any
	// claim of their own.
	if _, err := repo.Create(ctx, session.CreateOptions{ID: "taken"}); err == nil {
		t.Fatal("creating a session twice succeeded")
	}
	if _, err := repo.Create(ctx, session.CreateOptions{ID: "taken "}); err == nil {
		t.Fatal("creating a session under a colliding name succeeded")
	}

	sess, err := repo.Open(ctx, "taken")
	if err != nil {
		t.Fatalf("the original session is gone: %v", err)
	}
	if sess == nil {
		t.Fatal("Open returned no session")
	}
	sidecar := filepath.Join(dir, sanitizeSessionID("taken")+".meta.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the original's metadata was removed: %v", err)
	}
}

// The rollback must also stay out of the way of an ordinary success.
func TestCreateKeepsItsSidecarOnSuccess(t *testing.T) {
	dir := t.TempDir()
	repo := newRepo(t, dir)

	if _, err := repo.Create(context.Background(), session.CreateOptions{ID: "kept"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sanitizeSessionID("kept")+".meta.json")); err != nil {
		t.Fatalf("a successful create left no sidecar: %v", err)
	}
	list, err := repo.List(context.Background(), session.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "kept" {
		t.Fatalf("List = %+v, want the one session just created", list)
	}
}

func newRepo(t *testing.T, dir string) *Repo {
	t.Helper()
	repo, err := NewRepo(dir)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	return repo
}
