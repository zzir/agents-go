package sessions_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/sessions"
)

func newTaskStore(t *testing.T) *sessions.TaskStore {
	t.Helper()
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "tasks.db")
	_, db, err := sessions.NewSQLite(dsn, "unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sessions.CreateTaskSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return sessions.NewTaskStore(db)
}

func mkTask(id, parent, child string) *tasks.Task {
	return &tasks.Task{
		ID: id, RunID: id + "-run", Label: "job " + id,
		ParentSessionID: parent, ChildSessionID: child,
		Depth: 1, Status: tasks.StatusWorking,
		Inherit: []byte(`{"agent":"worker"}`),
	}
}

func TestSQLTaskStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)

	in := mkTask("t1", "parent", "child-1")
	if err := s.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != in.RunID || got.Depth != 1 || string(got.Inherit) != `{"agent":"worker"}` {
		t.Errorf("round trip lost fields: %+v", got)
	}
	byChild, err := s.ByChildSession(ctx, "child-1")
	if err != nil || byChild.ID != "t1" {
		t.Errorf("ByChildSession = %+v, %v", byChild, err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.ByChildSession(ctx, "nope"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The whole reason tasks require a transactional store: the database arbitrates
// between racing finalizers, not the process.
func TestSQLTaskStore_FinalizeIsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var wins int
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := tasks.StatusCompleted
			if i%2 == 0 {
				st = tasks.StatusFailed
			}
			won, err := s.Finalize(ctx, "t1", st, "s", "r")
			if err != nil {
				t.Error(err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d finalizers won, want exactly 1", wins)
	}
	// The winner's transition carries status, result and the debt together.
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Status.Terminal() || got.Result != "r" || got.NotifyState != tasks.NotifyPending {
		t.Errorf("task = %+v, want a complete terminal transition", got)
	}
}

// Notification bookkeeping must not move updated_at: for a terminal task that
// column is when it finished, and delivery can be much later.
func TestSQLTaskStore_DeliveryDoesNotTouchUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "child-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finalize(ctx, "t1", tasks.StatusCompleted, "s", "r"); err != nil {
		t.Fatal(err)
	}
	finished, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNotifyDelivered(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(finished.UpdatedAt) {
		t.Errorf("updated_at moved on delivery: %v → %v", finished.UpdatedAt, after.UpdatedAt)
	}
	if after.NotifyState != tasks.NotifyDelivered {
		t.Errorf("notify state = %q", after.NotifyState)
	}
}

func TestSQLTaskStore_Listings(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := s.Create(ctx, mkTask(id, "parent", "child-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Finalize(ctx, "t2", tasks.StatusCompleted, "done", "done"); err != nil {
		t.Fatal(err)
	}

	live, err := s.ListNonTerminal(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Errorf("%d live tasks, want 2", len(live))
	}
	pending, err := s.ListPendingNotify(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "t2" {
		t.Errorf("pending = %+v, want just t2", pending)
	}
	parents, err := s.PendingNotifyParents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 || parents[0] != "parent" {
		t.Errorf("parents = %v", parents)
	}
	all, err := s.ListByParent(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("%d tasks, want 3", len(all))
	}
}

// ListNonTerminal filters in SQL, so its idea of "terminal" is a copy of
// tasks.Status.Terminal rather than a call to it. This drives a task into each
// of the statuses below and asks whether the two still agree — one that changes
// sides upstream shows up here as a task listed as live after it finished (a
// teardown then tries to cancel it) or dropped while it is still running (a
// teardown leaves it behind).
//
// The list is written out because tasks exports no enumeration to walk, which
// is also this test's blind spot: a status ADDED upstream has to be added here
// and to terminalStatuses by hand.
func TestSQLTaskStore_ListNonTerminalMatchesStatusTerminal(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	all := []tasks.Status{
		tasks.StatusWorking, tasks.StatusInputRequired,
		tasks.StatusCompleted, tasks.StatusFailed, tasks.StatusCancelled,
	}

	want := map[string]bool{}
	for _, st := range all {
		id := string(st)
		if err := s.Create(ctx, mkTask(id, "parent", "child-"+id)); err != nil {
			t.Fatal(err)
		}
		switch st {
		case tasks.StatusWorking:
			// Created working already.
		case tasks.StatusInputRequired:
			if err := s.MarkInputRequired(ctx, id); err != nil {
				t.Fatal(err)
			}
		default:
			if _, err := s.Finalize(ctx, id, st, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != st {
			t.Fatalf("task %q is at %q, want %q", id, got.Status, st)
		}
		if !st.Terminal() {
			want[id] = true
		}
	}

	live, err := s.ListNonTerminal(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, task := range live {
		got[task.ID] = true
	}
	for _, st := range all {
		id := string(st)
		if got[id] != want[id] {
			t.Errorf("status %q: listed as live = %v, but Terminal() says live = %v", st, got[id], want[id])
		}
	}
}

// A task paused on an approval is not an orphan: its approval persists.
func TestSQLTaskStore_FailOrphansKeepsInputRequired(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "c1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, mkTask("t2", "parent", "c2")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInputRequired(ctx, "t2"); err != nil {
		t.Fatal(err)
	}

	n, err := s.FailOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("failed %d, want 1", n)
	}
	t2, err := s.Get(ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if t2.Status != tasks.StatusInputRequired {
		t.Errorf("status = %q, want input_required preserved", t2.Status)
	}
	t1, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if t1.Status != tasks.StatusFailed || t1.NotifyState != tasks.NotifyPending {
		t.Errorf("orphan = %+v, want failed and owing a wake-up", t1)
	}
}

func TestSQLTaskStore_ReclaimWorking(t *testing.T) {
	ctx := context.Background()
	s := newTaskStore(t)
	if err := s.Create(ctx, mkTask("t1", "parent", "c1")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInputRequired(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ReclaimWorking(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
	// A task that went terminal meanwhile cannot be reclaimed — the resume
	// must be abandoned.
	if _, err := s.Finalize(ctx, "t1", tasks.StatusCancelled, "", ""); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ReclaimWorking(ctx, "t1"); ok {
		t.Error("reclaimed a terminal task")
	}
}
