package sessions_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
)

// failed drives a task to the one status a retry may resume from.
func failed(t *testing.T, s interface {
	Finalize(context.Context, string, string, tasks.Status, string, string, json.RawMessage) (bool, error)
}, id string) {
	t.Helper()
	won, err := s.Finalize(context.Background(), id, id+"-run", tasks.StatusFailed, "boom", "boom", nil)
	if err != nil || !won {
		t.Fatalf("failing %s: won=%v err=%v", id, won, err)
	}
}

// The claim is one conditional UPDATE, so the database arbitrates between
// racing retries rather than the process — the same guarantee Finalize makes,
// and for the same reason: several hosts can share one store.
func TestSQLTaskStore_RetryClaimIsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}
	failed(t, s, "t1")

	var mu sync.Mutex
	var wins int
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			won, err := s.RetryClaim(ctx, "t1", "run-"+string(rune('a'+i)), 5)
			if err != nil {
				t.Error(err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d claims won, want exactly 1", wins)
	}

	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.StatusWorking {
		t.Errorf("status = %q, want working", got.Status)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 — one claim, one increment", got.Attempt)
	}
	if got.RunID == "t1-run" {
		t.Error("the claim kept the failed run's id")
	}
	if got.Summary != "" || got.Result != "" {
		t.Errorf("the previous attempt survived the claim: %+v", got)
	}
}

// A row bound to a session generation that is gone must stay dead. Reviving it
// would launch a run onto a session id that now answers to someone else.
func TestSQLTaskStore_RetryClaimRefusesADeadGeneration(t *testing.T) {
	ctx := context.Background()
	repo, store, db := taskRepo(t)
	if _, err := repo.Create(ctx, session.CreateOptions{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewInsert().Table("agent_tasks").
		Model(&map[string]any{
			"id": "t1", "run_id": "t1-run", "label": "stale",
			"parent_session_id": "s1", "parent_session_gen": "a-dead-generation",
			"child_session_id": "child-1", "child_session_gen": "a-dead-generation",
			"depth": 1, "attempt": 1, "status": string(tasks.StatusFailed),
			"created_at": "2020-01-01 00:00:00+00:00", "updated_at": "2020-01-01 00:00:00+00:00",
		}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	won, err := store.RetryClaim(ctx, "t1", "run2", 3)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("a dead incarnation's task was brought back to life")
	}
}
