package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// WithSessionTreeFenced fences the session AND the hidden sessions serving it
// (a live run on a task's child refuses the write), keeps a run from starting
// anywhere in the tree while fn runs, and drains the session's wake-up debts
// once the fence lifts.
func TestWithSessionTreeFencedCoversTheTreeAndDrains(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, _ := newTaskTestRunner(t)
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	child := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "task", Hidden: true}
	for _, s := range []*store.Session{parent, child} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := tasks.Create(ctx, &store.Task{ID: store.NewID(), RunID: store.NewID(), ParentSessionID: parent.ID, ChildSessionID: child.ID, Status: "working"}); err != nil {
		t.Fatal(err)
	}
	var busy ErrSessionBusy

	// A run live on the child — a task at work — refuses the write outright.
	seg, _, err := runner.hub.register("child-run", child.ID, "", "agent", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = runner.WithSessionTreeFenced(ctx, parent.ID, func() error { called = true; return nil })
	if !errors.As(err, &busy) || busy.RunID != "child-run" || called {
		t.Fatalf("fenced write over a live task run: err = %v, called = %v; want ErrSessionBusy{child-run} and fn unrun", err, called)
	}
	runner.hub.finish("child-run", false)
	seg.finalize()

	// A debt with no agent config is cancelled by a drain: proof one ran.
	if err := runner.Deps.Wakeups.Owe(ctx, &store.Wakeup{SessionID: parent.ID, Kind: store.WakeKindTask, Payload: "p"}); err != nil {
		t.Fatal(err)
	}
	errInside := errors.New("fn's own")
	err = runner.WithSessionTreeFenced(ctx, parent.ID, func() error {
		for _, id := range []string{parent.ID, child.ID} {
			if _, _, err := runner.hub.register(store.NewID(), id, "", "agent", "", nil); !errors.As(err, &busy) {
				t.Errorf("register on %s inside the fence: err = %v, want ErrSessionBusy", id, err)
			}
		}
		other := store.NewID()
		if s, _, err := runner.hub.register(other, store.NewID(), "", "agent", "", nil); err != nil {
			t.Errorf("register on an unrelated session inside the fence: %v", err)
		} else {
			runner.hub.unregister(other, s)
		}
		if pending, _ := runner.Deps.Wakeups.Pending(ctx, parent.ID); len(pending) != 1 {
			t.Errorf("the debt was drained inside the fence: %v", pending)
		}
		return errInside
	})
	if !errors.Is(err, errInside) {
		t.Fatalf("fn's error = %v, want it returned as is", err)
	}
	after := store.NewID()
	if s, _, err := runner.hub.register(after, child.ID, "", "agent", "", nil); err != nil {
		t.Fatalf("register on the child after the fence: %v", err)
	} else {
		runner.hub.unregister(after, s)
	}
	if pending, err := runner.Deps.Wakeups.Pending(ctx, parent.ID); err != nil || len(pending) != 0 {
		t.Fatalf("pending debts after the fence = %v (err %v); the release did not drain", pending, err)
	}
}
