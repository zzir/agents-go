// Package conformancetest holds the behavioral contract every tasks.Store
// implementation must satisfy, as one reusable test suite — the same shape as
// modelkit/conformancetest for Model implementations. A store passes this
// suite or it does not implement the interface, whatever its comments claim.
//
// Store-SPECIFIC behavior stays in each store's own tests — above all the SQL
// stores' session-generation predicates, which the in-memory store does not
// have.
package conformancetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/zzir/agents-go/agents/tasks"
)

// Run exercises the tasks.Store contract against fresh stores from newStore.
// Each subtest gets its own store, so implementations need no cross-test
// cleanup.
func Run(t *testing.T, newStore func(t *testing.T) tasks.Store) {
	t.Helper()
	ctx := context.Background()

	// mk returns a working task with stable ids per n — UUIDs, memoized by
	// name, since a backend may type its id columns (the server's PostgreSQL
	// does) and a literal "t1" is then a syntax error, not an id.
	ids := map[string]string{}
	name := func(s string) string {
		if ids[s] == "" {
			ids[s] = uuid.NewV7().String()
		}
		return ids[s]
	}
	mk := func(n int) *tasks.Task {
		id := fmt.Sprintf("t%d", n)
		return &tasks.Task{
			ID: name(id), RunID: name(id + "-run"), Label: "job " + id,
			ParentSessionID: name("parent-" + id), ChildSessionID: name("child-" + id),
			Status: tasks.StatusWorking,
		}
	}
	create := func(t *testing.T, s tasks.Store, n int) *tasks.Task {
		t.Helper()
		task := mk(n)
		if err := s.Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
		return task
	}

	t.Run("create and read back", func(t *testing.T) {
		s := newStore(t)
		task := mk(1)
		task.Kind, task.State = "sequence", json.RawMessage(`{"step":1}`)
		if err := s.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RunID != task.RunID || got.Status != tasks.StatusWorking || got.Label != task.Label {
			t.Fatalf("roundtrip lost fields: %+v", got)
		}
		// The host's own fields travel untouched: Kind as a name, State as the
		// exact bytes — a store must not re-encode what it does not read.
		if got.Kind != "sequence" || string(got.State) != `{"step":1}` {
			t.Fatalf("roundtrip lost the host's fields: kind %q state %s", got.Kind, got.State)
		}
		if _, err := s.Get(ctx, name("absent")); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("missing task: err = %v, want ErrNotFound", err)
		}
		if got, err := s.ByChildSession(ctx, task.ChildSessionID); err != nil || got.ID != task.ID {
			t.Fatalf("ByChildSession = %v, %v", got, err)
		}
	})

	t.Run("finalize is a run-bound CAS", func(t *testing.T) {
		s := newStore(t)
		task := create(t, s, 1)
		// The wrong run cannot end the attempt.
		if won, err := s.Finalize(ctx, task.ID, name("other-run"), tasks.StatusFailed, "s", "r", nil); err != nil || won {
			t.Fatalf("foreign finalize: won=%v err=%v, want a refusal", won, err)
		}
		// The ending may carry the job's final State, written in the same
		// transition; nil leaves it as it was.
		won, err := s.Finalize(ctx, task.ID, task.RunID, tasks.StatusCompleted, "sum", "res", json.RawMessage(`{"last":"pass"}`))
		if err != nil || !won {
			t.Fatalf("finalize: won=%v err=%v", won, err)
		}
		got, _ := s.Get(ctx, task.ID)
		if got.Status != tasks.StatusCompleted || got.Summary != "sum" || got.Result != "res" || string(got.State) != `{"last":"pass"}` {
			t.Fatalf("finalize did not land whole: %+v", got)
		}
		// Terminal is terminal: a second finalizer loses.
		if won, err := s.Finalize(ctx, task.ID, task.RunID, tasks.StatusFailed, "", "", nil); err != nil || won {
			t.Fatalf("re-finalize: won=%v err=%v, want a refusal", won, err)
		}
	})

	t.Run("retry claim reopens exactly one failed attempt", func(t *testing.T) {
		s := newStore(t)
		task := create(t, s, 1)
		// Working: nothing to resume.
		if won, err := s.RetryClaim(ctx, task.ID, name("run2"), 3); err != nil || won {
			t.Fatalf("claimed a working task: won=%v err=%v", won, err)
		}
		if _, err := s.Finalize(ctx, task.ID, task.RunID, tasks.StatusFailed, "boom", "boom", nil); err != nil {
			t.Fatal(err)
		}
		won, err := s.RetryClaim(ctx, task.ID, name("run2"), 3)
		if err != nil || !won {
			t.Fatalf("claim: won=%v err=%v", won, err)
		}
		got, _ := s.Get(ctx, task.ID)
		if got.Status != tasks.StatusWorking || got.RunID != name("run2") || got.AttemptNo() != 2 {
			t.Fatalf("claim = %s/%s attempt %d, want working/run2 attempt 2", got.Status, got.RunID, got.AttemptNo())
		}
		if got.Summary != "" || got.Result != "" {
			t.Fatalf("the failed attempt survived the claim: %+v", got)
		}
		// Unknown id is a different answer from a refused claim.
		if _, err := s.RetryClaim(ctx, name("absent"), name("run"), 3); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("missing task: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("retry claim enforces the ceiling in the store", func(t *testing.T) {
		s := newStore(t)
		task := create(t, s, 1)
		// Attempt 1 → 2 → 3, refused at a ceiling of 3.
		for want := 2; want <= 3; want++ {
			if _, err := s.Finalize(ctx, task.ID, currentRun(t, s, task.ID), tasks.StatusFailed, "boom", "", nil); err != nil {
				t.Fatal(err)
			}
			if won, err := s.RetryClaim(ctx, task.ID, name(fmt.Sprintf("run%d", want)), 3); err != nil || !won {
				t.Fatalf("claim to attempt %d: won=%v err=%v", want, won, err)
			}
			if got, _ := s.Get(ctx, task.ID); got.AttemptNo() != want {
				t.Fatalf("attempt = %d, want %d", got.AttemptNo(), want)
			}
		}
		if _, err := s.Finalize(ctx, task.ID, currentRun(t, s, task.ID), tasks.StatusFailed, "boom", "", nil); err != nil {
			t.Fatal(err)
		}
		if won, err := s.RetryClaim(ctx, task.ID, name("run4"), 3); err != nil || won {
			t.Fatalf("claim past the ceiling: won=%v err=%v, want a refusal", won, err)
		}
		if got, _ := s.Get(ctx, task.ID); got.AttemptNo() != 3 {
			t.Fatalf("attempt = %d, want it to stay at 3", got.AttemptNo())
		}
	})

	t.Run("a zero attempt column reads as the first attempt", func(t *testing.T) {
		s := newStore(t)
		task := mk(1)
		task.Attempt = 0 // an insert that never set the column
		if err := s.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finalize(ctx, task.ID, task.RunID, tasks.StatusFailed, "boom", "", nil); err != nil {
			t.Fatal(err)
		}
		if won, err := s.RetryClaim(ctx, task.ID, name("run2"), 3); err != nil || !won {
			t.Fatalf("claim: won=%v err=%v", won, err)
		}
		if got, _ := s.Get(ctx, task.ID); got.AttemptNo() != 2 {
			t.Fatalf("attempt = %d, want 2 — zero counted as the first", got.AttemptNo())
		}
	})

	t.Run("advance is a run-bound CAS that carries state", func(t *testing.T) {
		s := newStore(t)
		task := create(t, s, 1)
		// The wrong run cannot move the task on.
		if won, err := s.Advance(ctx, task.ID, name("other-run"), name("run2"), json.RawMessage(`{"step":2}`)); err != nil || won {
			t.Fatalf("foreign advance: won=%v err=%v, want a refusal", won, err)
		}
		won, err := s.Advance(ctx, task.ID, task.RunID, name("run2"), json.RawMessage(`{"step":2}`))
		if err != nil || !won {
			t.Fatalf("advance: won=%v err=%v", won, err)
		}
		got, _ := s.Get(ctx, task.ID)
		if got.RunID != name("run2") || string(got.State) != `{"step":2}` || got.Status != tasks.StatusWorking {
			t.Fatalf("advance did not land whole: %+v", got)
		}
		// A continuation is not a retry: the attempt count is untouched.
		if got.AttemptNo() != 1 {
			t.Fatalf("advance changed the attempt: %d", got.AttemptNo())
		}
		// The same run id rewrites State in place — how a host records what it
		// learns at launch.
		if won, err := s.Advance(ctx, task.ID, name("run2"), name("run2"), json.RawMessage(`{"step":2,"launched":true}`)); err != nil || !won {
			t.Fatalf("in-place advance: won=%v err=%v", won, err)
		}
		if got, _ := s.Get(ctx, task.ID); string(got.State) != `{"step":2,"launched":true}` {
			t.Fatalf("in-place advance did not rewrite the state: %s", got.State)
		}
		// A nil state leaves State as it is — the rule Finalize follows too.
		if won, err := s.Advance(ctx, task.ID, name("run2"), name("run2b"), nil); err != nil || !won {
			t.Fatalf("advance with nil state: won=%v err=%v", won, err)
		}
		if got, _ := s.Get(ctx, task.ID); got.RunID != name("run2b") || string(got.State) != `{"step":2,"launched":true}` {
			t.Fatalf("a nil state must not clear the state: %+v", got)
		}
		if won, err := s.Advance(ctx, task.ID, name("run2b"), name("run2"), nil); err != nil || !won {
			t.Fatalf("advance back: won=%v err=%v", won, err)
		}
		// Only a WORKING task advances: paused and terminal rows refuse.
		if err := s.MarkInputRequired(ctx, task.ID, name("run2")); err != nil {
			t.Fatal(err)
		}
		if won, err := s.Advance(ctx, task.ID, name("run2"), name("run3"), nil); err != nil || won {
			t.Fatalf("advanced a paused task: won=%v err=%v", won, err)
		}
		if _, err := s.Finalize(ctx, task.ID, name("run2"), tasks.StatusCancelled, "", "", nil); err != nil {
			t.Fatal(err)
		}
		if won, err := s.Advance(ctx, task.ID, name("run2"), name("run3"), nil); err != nil || won {
			t.Fatalf("advanced a terminal task: won=%v err=%v", won, err)
		}
		if _, err := s.Advance(ctx, name("absent"), name("run"), name("run2"), nil); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("missing task: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("release retry claim rolls the attempt back", func(t *testing.T) {
		s := newStore(t)
		task := create(t, s, 1)
		if _, err := s.Finalize(ctx, task.ID, task.RunID, tasks.StatusFailed, "boom", "", nil); err != nil {
			t.Fatal(err)
		}
		if won, err := s.RetryClaim(ctx, task.ID, name("run2"), 3); err != nil || !won {
			t.Fatalf("claim: won=%v err=%v", won, err)
		}
		// Only the claim's owner can release it.
		if won, err := s.ReleaseRetryClaim(ctx, task.ID, name("other-run"), "s", "r"); err != nil || won {
			t.Fatalf("foreign release: won=%v err=%v, want a refusal", won, err)
		}
		won, err := s.ReleaseRetryClaim(ctx, task.ID, name("run2"), "never started", "never started")
		if err != nil || !won {
			t.Fatalf("release: won=%v err=%v", won, err)
		}
		got, _ := s.Get(ctx, task.ID)
		if got.Status != tasks.StatusFailed || got.AttemptNo() != 1 {
			t.Fatalf("after release: %s attempt %d, want failed attempt 1", got.Status, got.AttemptNo())
		}
		if got.Summary != "never started" {
			t.Fatalf("release did not record the failure: %+v", got)
		}
		// Released, the row is failed — nothing left to undo.
		if won, err := s.ReleaseRetryClaim(ctx, task.ID, name("run2"), "s", "r"); err != nil || won {
			t.Fatalf("double release: won=%v err=%v, want a refusal", won, err)
		}
	})

	t.Run("pause and reclaim are attempt-bound", func(t *testing.T) {
		s := newStore(t)
		task := create(t, s, 1)
		// A stale attempt's mark is a silent no-op.
		if err := s.MarkInputRequired(ctx, task.ID, name("other-run")); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, task.ID); got.Status != tasks.StatusWorking {
			t.Fatalf("stale mark paused the task: %s", got.Status)
		}
		if err := s.MarkInputRequired(ctx, task.ID, task.RunID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, task.ID); got.Status != tasks.StatusInputRequired {
			t.Fatalf("mark did not pause: %s", got.Status)
		}
		// A stale attempt cannot reclaim; the current one can, once.
		if ok, err := s.ReclaimWorking(ctx, task.ID, name("other-run")); err != nil || ok {
			t.Fatalf("stale reclaim: ok=%v err=%v, want a refusal", ok, err)
		}
		if ok, err := s.ReclaimWorking(ctx, task.ID, task.RunID); err != nil || !ok {
			t.Fatalf("reclaim: ok=%v err=%v", ok, err)
		}
		// Terminal beats a late reclaim.
		if _, err := s.Finalize(ctx, task.ID, task.RunID, tasks.StatusCancelled, "", "", nil); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.ReclaimWorking(ctx, task.ID, task.RunID); ok {
			t.Fatal("reclaimed a terminal task")
		}
		if _, err := s.ReclaimWorking(ctx, name("absent"), name("run")); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("missing task: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("fail orphans keeps paused tasks", func(t *testing.T) {
		s := newStore(t)
		working, paused := create(t, s, 1), create(t, s, 2)
		if err := s.MarkInputRequired(ctx, paused.ID, paused.RunID); err != nil {
			t.Fatal(err)
		}
		orphans, err := s.FailOrphans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The rows come BACK: each parent still has to be told, and the caller
		// is the only one who can arrange that.
		if len(orphans) != 1 || orphans[0].ID != working.ID {
			t.Fatalf("orphans = %+v, want just the working task — the paused one is not an orphan", orphans)
		}
		if got, _ := s.Get(ctx, working.ID); got.Status != tasks.StatusFailed {
			t.Fatalf("orphan = %+v, want failed", got)
		}
		if got, _ := s.Get(ctx, paused.ID); got.Status != tasks.StatusInputRequired {
			t.Fatalf("paused task = %s, want input_required preserved", got.Status)
		}
	})

	t.Run("listings and delete", func(t *testing.T) {
		s := newStore(t)
		a, b := mk(1), mk(2)
		b.ParentSessionID = a.ParentSessionID
		for _, task := range []*tasks.Task{a, b} {
			if err := s.Create(ctx, task); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Finalize(ctx, a.ID, a.RunID, tasks.StatusCompleted, "", "", nil); err != nil {
			t.Fatal(err)
		}
		live, err := s.ListNonTerminal(ctx, a.ParentSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 1 || live[0].ID != b.ID {
			t.Fatalf("non-terminal = %+v, want just t2", live)
		}
		all, err := s.ListByParent(ctx, a.ParentSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 || all[0].ID != b.ID {
			t.Fatalf("by parent = %+v, want [t2 t1] newest first", all)
		}
		if err := s.Delete(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, a.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("deleted task: err = %v, want ErrNotFound", err)
		}
	})
}

// currentRun reads the task's live run id — the ceiling walk re-finalizes
// whichever attempt the row is on.
func currentRun(t *testing.T, s tasks.Store, id string) string {
	t.Helper()
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.RunID
}
