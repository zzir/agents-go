package sessions_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
)

// failed drives a task to the one status a retry may resume from.
func failed(t *testing.T, s interface {
	Finalize(context.Context, string, string, tasks.Status, string, string) (bool, error)
}, id string) {
	t.Helper()
	won, err := s.Finalize(context.Background(), id, id+"-run", tasks.StatusFailed, "boom", "boom")
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
	if got.Summary != "" || got.Result != "" || got.NotifyState != tasks.NotifyNone {
		t.Errorf("the previous attempt survived the claim: %+v", got)
	}
}

// The ceiling lives in the WHERE clause, not only in the Manager that checked
// it first: two processes asking at once must not both get an attempt.
func TestSQLTaskStore_RetryClaimEnforcesTheCeiling(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}

	// Attempt 1 → 2 → 3, refused at a ceiling of 3.
	for want := 2; want <= 3; want++ {
		failed(t, s, "t1")
		won, err := s.RetryClaim(ctx, "t1", "t1-run", 3)
		if err != nil || !won {
			t.Fatalf("claim to attempt %d: won=%v err=%v", want, won, err)
		}
		got, err := s.Get(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Attempt != want {
			t.Fatalf("attempt = %d, want %d", got.Attempt, want)
		}
	}
	failed(t, s, "t1")
	if won, err := s.RetryClaim(ctx, "t1", "t1-run", 3); err != nil || won {
		t.Errorf("claim past the ceiling: won=%v err=%v", won, err)
	}
	if got, err := s.Get(ctx, "t1"); err != nil || got.Attempt != 3 {
		t.Errorf("attempt = %+v, want it to stay at 3", got)
	}
}

// A zero attempt column is a row written before retries existed: it had one
// attempt, and the claim must count it rather than starting over.
func TestSQLTaskStore_RetryClaimReadsZeroAsTheFirstAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	in := mkTask("t1", "parent", "child-1")
	in.Attempt = 0
	if err := s.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	failed(t, s, "t1")

	won, err := s.RetryClaim(ctx, "t1", "run2", 3)
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 — the zero row had already had one", got.Attempt)
	}
}

// Not-found and cannot-claim are different answers, and both shipped stores
// must give the same one: a caller written against the in-memory store has to
// be right against this one.
func TestSQLTaskStore_RetryClaimSeparatesMissingFromRefused(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}

	// Working, so not claimable — but it exists.
	won, err := s.RetryClaim(ctx, "t1", "run2", 3)
	if err != nil {
		t.Errorf("refusing a working task reported an error: %v", err)
	}
	if won {
		t.Error("claimed a working task")
	}
	if _, err := s.RetryClaim(ctx, "nope", "run2", 3); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
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
			"notify_state": string(tasks.NotifyPending),
			"created_at":   "2020-01-01 00:00:00+00:00", "updated_at": "2020-01-01 00:00:00+00:00",
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

// The notify writes carry the same attempt bound as Finalize: a consume
// decided against the previous attempt must not swallow the debt of the one
// that replaced it.
func TestSQLTaskStore_NotifyWritesAreBoundToTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}
	failed(t, s, "t1")
	if won, err := s.RetryClaim(ctx, "t1", "run2", 3); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if won, err := s.Finalize(ctx, "t1", "run2", tasks.StatusCompleted, "done", "done"); err != nil || !won {
		t.Fatalf("finalizing the new attempt: won=%v err=%v", won, err)
	}

	// The late arrivals, naming the attempt that is gone.
	if err := s.ConsumeNotify(ctx, "t1", "t1-run"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNotifyDelivered(ctx, "t1", "t1-run"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NotifyState != tasks.NotifyPending {
		t.Errorf("notify state = %q, want the new attempt's debt untouched", got.NotifyState)
	}
}

// A stop that read the row before a retry names the attempt it saw, and must
// lose: the task it would cancel is already over, and the live one would keep
// running with its own result discarded.
func TestSQLTaskStore_FinalizeIsBoundToTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}
	failed(t, s, "t1")
	if won, err := s.RetryClaim(ctx, "t1", "run2", 3); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}

	won, err := s.Finalize(ctx, "t1", "t1-run", tasks.StatusCancelled, "stopped", "")
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("a finalizer naming the previous attempt won")
	}
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.StatusWorking {
		t.Errorf("status = %q, want the new attempt still working", got.Status)
	}
}
