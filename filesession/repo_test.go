package filesession_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/filesession"
)

func TestRepo(t *testing.T) {
	ctx := context.Background()
	repo, err := filesession.NewRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runRepoContract(ctx, t, repo)
}

// runRepoContract is the behavior every SessionRepo must have, whatever it is
// backed by.
func runRepoContract(ctx context.Context, t *testing.T, repo session.Repo) {
	t.Helper()

	visible, err := repo.Create(ctx, session.CreateOptions{ID: "chat-1", Title: "A chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, session.CreateOptions{ID: "task-1", Hidden: true}); err != nil {
		t.Fatal(err)
	}

	// Hidden sessions serve another session; a listing leaves them out so every
	// caller stops maintaining that filter — and stops forgetting it.
	listed, err := repo.List(ctx, session.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "chat-1" {
		t.Fatalf("List = %+v, want only the visible session", listed)
	}
	if listed[0].Title != "A chat" {
		t.Errorf("title = %q, want it preserved", listed[0].Title)
	}

	all, err := repo.List(ctx, session.ListOptions{IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List(IncludeHidden) = %d sessions, want 2", len(all))
	}

	// A created session is listed even with nothing in it: existence is not
	// inferred from contents.
	if listed[0].EntryCount != 0 {
		t.Errorf("a fresh session reports %d entries", listed[0].EntryCount)
	}

	if err := visible.AppendItems(ctx, agents.InputItemsFromText("hello"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	reopened, err := repo.Open(ctx, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	items, err := reopened.ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("reopened session holds %d items, want 1", len(items))
	}

	// Opening one that does not exist must NOT look like an empty conversation:
	// a run would start over instead of continuing.
	if _, err := repo.Open(ctx, "no-such-session"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Open(missing) err = %v, want ErrSessionNotFound", err)
	}

	if err := repo.Delete(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, "chat-1"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("a deleted session still opens: %v", err)
	}
	after, _ := repo.List(ctx, session.ListOptions{IncludeHidden: true})
	if len(after) != 1 {
		t.Errorf("after delete, List = %d sessions, want 1", len(after))
	}

	// Deleting one that is already gone is not an error — it is the state the
	// caller asked for.
	if err := repo.Delete(ctx, "chat-1"); err != nil {
		t.Errorf("deleting an absent session should be a no-op, got %v", err)
	}
}

// A repeat Create must not hand back a session that already holds a previous
// one's entries: the .jsonl beside the sidecar is keyed on the same filename,
// so overwriting the sidecar adopts that history rather than starting fresh.
func TestRepoCreateRefusesADuplicateID(t *testing.T) {
	ctx := context.Background()
	repo, err := filesession.NewRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first, err := repo.Create(ctx, session.CreateOptions{ID: "dup", Title: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(ctx, userEntry(t, "PRIOR")); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Create(ctx, session.CreateOptions{ID: "dup"}); err == nil {
		t.Fatal("creating an existing session id succeeded; it would inherit the old history")
	}
}

// sanitizeSessionID folds anything outside [A-Za-z0-9._-] to "_", so distinct
// ids can land on one file. Sharing it would let one session read and delete
// another's history.
func TestRepoRejectsSanitizationCollisions(t *testing.T) {
	ctx := context.Background()
	repo, err := filesession.NewRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	a, err := repo.Create(ctx, session.CreateOptions{ID: "team a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Append(ctx, userEntry(t, "BELONGS-TO-A")); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Create(ctx, session.CreateOptions{ID: "team+a"}); err == nil {
		t.Fatal(`"team+a" was created alongside "team a"; they share one file`)
	}
	// Nor may the colliding name reach the existing session by another route.
	if _, err := repo.Open(ctx, "team+a"); err == nil {
		t.Fatal(`Open("team+a") returned the session belonging to "team a"`)
	}
	if err := repo.Delete(ctx, "team+a"); err == nil {
		t.Fatal(`Delete("team+a") would have destroyed "team a"`)
	}
	if _, err := repo.Open(ctx, "team a"); err != nil {
		t.Fatalf("the real session became unreachable: %v", err)
	}
}

// An id with no usable filename form would collide with every other such id.
func TestRepoRejectsIDWithNoFilenameForm(t *testing.T) {
	repo, err := filesession.NewRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"...", "   ", ".."} {
		if _, err := repo.Create(context.Background(), session.CreateOptions{ID: id}); err == nil {
			t.Errorf("Create(%q) succeeded; it has no filename to live under", id)
		}
	}
}

// The package has three constructors and only one of them cares whether the
// session already exists. That is why the store constructors are New and
// NewAtPath rather than New and Open: a package-level Open next to Repo.Open
// would read like os.Open — "the file had better be there" — while in fact it
// hands back a store over nothing at all.
func TestMissingSessionOnlyFailsThroughTheRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for name, open := range map[string]func() (session.Storage, error){
		"New": func() (session.Storage, error) { return filesession.New(dir, "never-created") },
		"NewAtPath": func() (session.Storage, error) {
			return filesession.NewAtPath(filepath.Join(dir, "never-created-either.jsonl"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := open()
			if err != nil {
				t.Fatalf("%s over a missing file: %v", name, err)
			}
			entries, err := store.Entries(ctx, session.Cursor{})
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("Entries = %d, want 0", len(entries))
			}
		})
	}

	repo, err := filesession.NewRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, "never-created"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Repo.Open on a missing session = %v, want ErrNotFound", err)
	}
}

func userEntry(t *testing.T, text string) session.Entry {
	t.Helper()
	it, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"` + text + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := session.NewItemEntry(it, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
