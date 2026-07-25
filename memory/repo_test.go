package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/memory"
)

func TestRepo(t *testing.T) {
	ctx := context.Background()
	repo, err := memory.NewRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runRepoContract(ctx, t, repo)
}

// runRepoContract is the behavior every SessionRepo must have, whatever it is
// backed by.
func runRepoContract(ctx context.Context, t *testing.T, repo agents.SessionRepo) {
	t.Helper()

	visible, err := repo.Create(ctx, agents.CreateOptions{ID: "chat-1", Title: "A chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, agents.CreateOptions{ID: "task-1", Hidden: true}); err != nil {
		t.Fatal(err)
	}

	// Hidden sessions serve another session; a listing leaves them out so every
	// caller stops maintaining that filter — and stops forgetting it.
	listed, err := repo.List(ctx, agents.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "chat-1" {
		t.Fatalf("List = %+v, want only the visible session", listed)
	}
	if listed[0].Title != "A chat" {
		t.Errorf("title = %q, want it preserved", listed[0].Title)
	}

	all, err := repo.List(ctx, agents.ListOptions{IncludeHidden: true})
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
	items, err := reopened.ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("reopened session holds %d items, want 1", len(items))
	}

	// Opening one that does not exist must NOT look like an empty conversation:
	// a run would start over instead of continuing.
	if _, err := repo.Open(ctx, "no-such-session"); !errors.Is(err, agents.ErrSessionNotFound) {
		t.Errorf("Open(missing) err = %v, want ErrSessionNotFound", err)
	}

	if err := repo.Delete(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, "chat-1"); !errors.Is(err, agents.ErrSessionNotFound) {
		t.Errorf("a deleted session still opens: %v", err)
	}
	after, _ := repo.List(ctx, agents.ListOptions{IncludeHidden: true})
	if len(after) != 1 {
		t.Errorf("after delete, List = %d sessions, want 1", len(after))
	}

	// Deleting one that is already gone is not an error — it is the state the
	// caller asked for.
	if err := repo.Delete(ctx, "chat-1"); err != nil {
		t.Errorf("deleting an absent session should be a no-op, got %v", err)
	}
}
