package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// A job of several runs: the Continue hook keeps the task going, the Manager
// claims each transition and launches the next run with the state the hook
// returned, and the ending is reported once, at the end.
func TestContinue_ChainsRunsUnderOneTask(t *testing.T) {
	ctx := context.Background()
	steps := 0
	h := newHarness(t, func(c *Config) {
		c.Continue = func(_ context.Context, task *Task, out RunOutcome) (*Continuation, error) {
			steps++
			if steps >= 3 {
				return nil, nil // the third run ends it
			}
			return &Continuation{
				Input: "next",
				State: json.RawMessage(fmt.Sprintf(`{"step":%d}`, steps+1)),
			}, nil
		}
	})
	info, err := h.m.Spawn(ctx, SpawnRequest{
		ParentSessionID: "parent", AgentName: "worker", Input: "go", Label: "seq",
		Kind: "sequence", State: json.RawMessage(`{"step":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := h.get(t, info.TaskID)
	if first.Kind != "sequence" || string(first.State) != `{"step":1}` {
		t.Fatalf("spawn dropped the host's fields: %+v", first)
	}
	if got := h.launcher.all(); len(got) != 1 || got[0].TaskID != info.TaskID || got[0].Kind != "sequence" {
		t.Fatalf("first launch = %+v, want the task's id and kind on it", got)
	}

	child := first.ChildSessionID
	// Run 1 ends: the task moves on, still working, on a new run.
	h.m.OnRunFinished(ctx, child, RunOutcome{RunID: first.RunID, Status: StatusCompleted, Text: "one"})
	second := h.get(t, info.TaskID)
	if second.Status != StatusWorking || second.RunID == first.RunID {
		t.Fatalf("after run 1: %s on %s, want working on a new run", second.Status, second.RunID)
	}
	if string(second.State) != `{"step":2}` || second.AttemptNo() != 1 {
		t.Fatalf("after run 1: state %s attempt %d, want step 2 on attempt 1", second.State, second.AttemptNo())
	}
	launches := h.launcher.all()
	if len(launches) != 2 || launches[1].RunID != second.RunID || launches[1].Input != "next" || string(launches[1].State) != `{"step":2}` {
		t.Fatalf("second launch = %+v, want the new run with the hook's input and state", launches[1:])
	}
	if len(h.reportedFinished()) != 0 {
		t.Fatal("a continuation was reported as an ending")
	}

	// Run 2 ends: one more.
	h.m.OnRunFinished(ctx, child, RunOutcome{RunID: second.RunID, Status: StatusCompleted, Text: "two"})
	third := h.get(t, info.TaskID)
	if third.Status != StatusWorking || third.RunID == second.RunID {
		t.Fatalf("after run 2: %s on %s, want working on a new run", third.Status, third.RunID)
	}
	// Run 3 ends: the hook lets it end, with the last run's outcome.
	h.m.OnRunFinished(ctx, child, RunOutcome{RunID: third.RunID, Status: StatusCompleted, Text: "three"})
	done := h.get(t, info.TaskID)
	if done.Status != StatusCompleted || done.Result != "three" {
		t.Fatalf("final = %s %q, want completed with the last run's text", done.Status, done.Result)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || fin[0].RunID != third.RunID {
		t.Fatalf("finished reports = %+v, want exactly the ending, on the last run", fin)
	}
}

// A hook that errors ends the task as failed, with the error as the reason —
// the parent hears it like any failure.
func TestContinue_ErrorFailsTheTask(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return nil, errors.New("the sequence is looping")
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusCompleted, Text: "fine"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusFailed || got.Summary != "the sequence is looping" {
		t.Fatalf("task = %s %q, want failed with the hook's reason", got.Status, got.Summary)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || fin[0].Status != StatusFailed {
		t.Fatalf("finished reports = %+v, want one failure", fin)
	}
}

// A continuation whose run cannot start ends the task as failed: the job
// stopped, and silence would leave the parent waiting on it forever.
func TestContinue_LaunchFailureFailsTheTask(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return &Continuation{Input: "next"}, nil
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	h.launcher.mu.Lock()
	h.launcher.err = errors.New("no slot")
	h.launcher.mu.Unlock()
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusCompleted})
	got := h.get(t, info.TaskID)
	if got.Status != StatusFailed || got.RunID == task.RunID {
		t.Fatalf("task = %s on %s, want failed on the run that never started", got.Status, got.RunID)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || fin[0].Status != StatusFailed {
		t.Fatalf("finished reports = %+v, want one failure", fin)
	}
}

// A Continuation without an Input ENDS the task, and its State lands in the
// same write as the ending — completed with the run's outcome, or failed with
// the Continuation's Err — so the record of how the job ended cannot fall out
// of step with the status.
func TestContinue_EndingWritesTheFinalState(t *testing.T) {
	ctx := context.Background()
	final := json.RawMessage(`{"steps":3,"last":"pass"}`)
	var end error
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return &Continuation{State: final, Err: end}, nil
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusCompleted, Text: "done"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusCompleted || string(got.State) != string(final) {
		t.Fatalf("task = %s state %s, want completed with the ending's state", got.Status, got.State)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || string(fin[0].State) != string(final) {
		t.Fatalf("finished report = %+v, want the final state in hand", fin)
	}

	// With Err, the ending is a failure with that reason — state still written.
	end = errors.New("budget exhausted: 3 of 3 steps")
	info2 := h.spawn(t)
	task2 := h.get(t, info2.TaskID)
	h.m.OnRunFinished(ctx, task2.ChildSessionID, RunOutcome{RunID: task2.RunID, Status: StatusCompleted})
	got2 := h.get(t, info2.TaskID)
	if got2.Status != StatusFailed || got2.Summary != end.Error() || string(got2.State) != string(final) {
		t.Fatalf("task = %s %q state %s, want failed with the reason and the state", got2.Status, got2.Summary, got2.State)
	}
}

// A paused row (input_required, its approval never reclaimed) reporting a
// terminal outcome ends — finalized as it is, not dropped by a continuation
// whose claim could never win on a row that is not working.
func TestContinue_NotConsultedOnAPausedRow(t *testing.T) {
	ctx := context.Background()
	var asked atomic.Int32
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			asked.Add(1)
			return &Continuation{Input: "next"}, nil
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	// The run pauses on an approval, then — a host that skipped the reclaim —
	// reports its ending on the paused row.
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusInputRequired})
	if h.get(t, info.TaskID).Status != StatusInputRequired {
		t.Fatal("setup: the row must be paused")
	}
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusCompleted, Text: "done anyway"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusCompleted || got.Result != "done anyway" {
		t.Fatalf("task = %s %q, want the ending finalized on the paused row", got.Status, got.Result)
	}
	if asked.Load() != 0 {
		t.Fatalf("Continue was consulted %d times on a row that is not working", asked.Load())
	}
	if fin := h.reportedFinished(); len(fin) != 1 {
		t.Fatalf("finished reports = %d, want the ending reported", len(fin))
	}
}

// An outcome that does not name its run cannot be told from a redelivery once
// the task has moved on: Continue is not consulted for it (warned), and the
// ending is applied to whichever run the row names — the plain-task rule.
func TestContinue_NeedsARunID(t *testing.T) {
	ctx := context.Background()
	var asked atomic.Int32
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			asked.Add(1)
			return &Continuation{Input: "next"}, nil
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{Status: StatusCompleted, Text: "done"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusCompleted || asked.Load() != 0 {
		t.Fatalf("task = %s, asked %d — want the ending applied and Continue skipped", got.Status, asked.Load())
	}
}

// advanceFailingStore refuses every Advance — a database that fails between a
// run's end and the next run's claim.
type advanceFailingStore struct{ Store }

func (advanceFailingStore) Advance(context.Context, string, string, string, json.RawMessage) (bool, error) {
	return false, errors.New("database is locked")
}

// A continuation whose transition cannot be WRITTEN ends the task failed on the
// run that just finished — a row left working on an ended run would be a
// zombie until a restart's sweep — and the parent hears of it.
func TestContinue_AdvanceFailureFailsTheTask(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Store = advanceFailingStore{c.Store}
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return &Continuation{Input: "next"}, nil
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusCompleted, Text: "step one done"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusFailed || got.RunID != task.RunID || !strings.Contains(got.Summary, "could not advance") {
		t.Fatalf("task = %s on %s %q, want failed on the finished run with the reason", got.Status, got.RunID, got.Summary)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || fin[0].Status != StatusFailed {
		t.Fatalf("finished reports = %+v, want one failure", fin)
	}
}

// The hook is not consulted about a cancellation, nor about an attempt the task
// has moved past: a stop ends the task, and a stale outcome moves nothing.
func TestContinue_SkipsCancellationsAndStaleAttempts(t *testing.T) {
	ctx := context.Background()
	asked := 0
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			asked++
			return &Continuation{Input: "next"}, nil
		}
	})
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	// A superseded run's outcome: nothing to continue.
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: "old-run", Status: StatusCompleted})
	if asked != 0 {
		t.Fatal("asked about a run the task is not on")
	}
	if got := h.get(t, info.TaskID); got.RunID != task.RunID || got.Status != StatusWorking {
		t.Fatalf("a stale outcome moved the task: %+v", got)
	}
	// A cancellation ends it whatever the hook would say.
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusCancelled})
	if asked != 0 {
		t.Fatal("asked about a cancellation")
	}
	if got := h.get(t, info.TaskID); got.Status != StatusCancelled {
		t.Fatalf("task = %s, want cancelled", got.Status)
	}
}

// A retry of a multi-run task relaunches with the task's identity — the launcher
// gets the kind and the state it left off in, not a blank request.
func TestContinue_RetryCarriesKindAndState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info, err := h.m.Spawn(ctx, SpawnRequest{
		ParentSessionID: "parent", AgentName: "worker", Input: "go", Label: "seq",
		Kind: "sequence", State: json.RawMessage(`{"step":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	task := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, task.ChildSessionID, RunOutcome{RunID: task.RunID, Status: StatusFailed, Err: "boom"})
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	launches := h.launcher.all()
	last := launches[len(launches)-1]
	if last.TaskID != info.TaskID || last.Kind != "sequence" || string(last.State) != `{"step":2}` {
		t.Fatalf("retry launch = %+v, want the task's kind and state on it", last)
	}
}

// The launch window on the continuation path: Advance claims the row for the
// next run before the host hears of it, and a stop landing in between cancels
// a run the Stopper cannot reach yet. The settle catches it — the row stays
// cancelled, the run started anyway is stopped, and the ending is the
// cancellation (delivered, never woken for), not an outcome of the raced run.
func TestContinue_StopInsideTheLaunchWindow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return &Continuation{Input: "next", State: json.RawMessage(`{"step":2}`)}, nil
		}
	})
	info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "go", Label: "seq"})
	if err != nil {
		t.Fatal(err)
	}
	first := h.get(t, info.TaskID)

	var stopped *Info
	h.launcher.beforeLaunch = func(LaunchRequest) {
		h.launcher.beforeLaunch = nil
		var serr error
		if stopped, serr = h.m.Stop(ctx, info.TaskID, false); serr != nil {
			t.Errorf("staging the interleaved stop: %v", serr)
		}
	}
	h.m.OnRunFinished(ctx, first.ChildSessionID, RunOutcome{RunID: first.RunID, Status: StatusCompleted, Text: "one"})

	if stopped == nil || stopped.Status != StatusCancelled {
		t.Fatalf("the staged stop did not cancel: %+v", stopped)
	}
	row := h.get(t, info.TaskID)
	if row.Status != StatusCancelled || row.RunID == first.RunID {
		t.Fatalf("row = %s on %s, want cancelled on the advanced run", row.Status, row.RunID)
	}
	launched := h.launcher.all()
	if len(launched) != 2 {
		t.Fatalf("launches = %d, want the first run and the one the stop raced", len(launched))
	}
	h.mu.Lock()
	stoppedRuns := slices.Clone(h.stopped)
	h.mu.Unlock()
	if !slices.Contains(stoppedRuns, launched[1].RunID) {
		t.Errorf("run %s was launched for a cancelled task and never stopped (stopped: %v)", launched[1].RunID, stoppedRuns)
	}
	if fin := h.reportedFinished(); len(fin) != 0 {
		t.Fatalf("finished reports = %+v, want none: a stop is not woken for", fin)
	}
	if del := h.reportedDelivered(); len(del) != 1 || del[0].Status != StatusCancelled {
		t.Fatalf("delivered reports = %+v, want exactly the cancellation", del)
	}
}

// A Continuation with no State keeps the recorded one — on the row (Advance)
// AND on the launch the next run gets: the two must not disagree.
func TestContinue_NilStateKeepsTheRecordedOneOnTheLaunch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return &Continuation{Input: "next"}, nil // no State
		}
	})
	info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "go", State: json.RawMessage(`{"step":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	first := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, first.ChildSessionID, RunOutcome{RunID: first.RunID, Status: StatusCompleted})
	if row := h.get(t, info.TaskID); string(row.State) != `{"step":1}` {
		t.Fatalf("row state = %s, want the recorded one kept", row.State)
	}
	launches := h.launcher.all()
	if len(launches) != 2 || string(launches[1].State) != `{"step":1}` {
		t.Fatalf("second launch state = %q, want the recorded state handed to the launcher", launches[1].State)
	}
}

// A graceful stop ends the task whatever the turn it let finish ended with:
// a turn that FAILS after the stop was granted is not consulted for a next
// run either — the person's stop is the decision.
func TestContinue_NotConsultedAfterAGracefulStop(t *testing.T) {
	ctx := context.Background()
	asked := 0
	h := newHarness(t, func(c *Config) {
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			asked++
			return &Continuation{Input: "next"}, nil
		}
	})
	info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "go"})
	if err != nil {
		t.Fatal(err)
	}
	first := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, first.ChildSessionID, RunOutcome{RunID: first.RunID, Status: StatusFailed, Err: "boom", GracefulStop: true})
	if asked != 0 {
		t.Fatalf("Continue asked %d times after a graceful stop", asked)
	}
	if row := h.get(t, info.TaskID); !row.Status.Terminal() {
		t.Fatalf("row = %s, want the task ended", row.Status)
	}
	if n := len(h.launcher.all()); n != 1 {
		t.Fatalf("%d launches, want the first run only", n)
	}
}

// A pause of the SAME run delivered inside the Continue window — after the
// snapshot that said working, before the claim — puts the row back to
// input_required, where the transition cannot be won. The run has ended, so
// the ending is finalized on it (failed: it could not go on) rather than the
// row stranded on a run nobody will resume.
func TestContinue_StalePauseInsideTheWindowDoesNotStrandTheRow(t *testing.T) {
	ctx := context.Background()
	var h *harness
	var child string
	h = newHarness(t, func(c *Config) {
		c.Continue = func(_ context.Context, task *Task, _ RunOutcome) (*Continuation, error) {
			// The host's late pause report of the run whose ending is being
			// decided lands while the hook runs.
			h.m.OnRunFinished(ctx, child, RunOutcome{RunID: task.RunID, Status: StatusInputRequired})
			return &Continuation{Input: "next"}, nil
		}
	})
	info := h.spawn(t)
	first := h.get(t, info.TaskID)
	child = first.ChildSessionID
	h.m.OnRunFinished(ctx, child, RunOutcome{RunID: first.RunID, Status: StatusCompleted, Text: "one"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusFailed || got.RunID != first.RunID || !strings.Contains(got.Summary, "could not advance") {
		t.Fatalf("task = %s on %s %q, want failed on the finished run rather than left paused", got.Status, got.RunID, got.Summary)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || fin[0].Status != StatusFailed {
		t.Fatalf("finished reports = %+v, want the parent told once", fin)
	}
	if n := len(h.launcher.all()); n != 1 {
		t.Fatalf("launches = %d, want no second run for a claim that was not won", n)
	}
}

// A hook that never says stop meets the continuation ceiling: at the bound
// the task ends failed instead of chaining forever, and a retry starts the
// count over.
func TestContinue_CeilingEndsAnEndlessChain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.MaxContinuations = 2
		c.Continue = func(context.Context, *Task, RunOutcome) (*Continuation, error) {
			return &Continuation{Input: "again"}, nil
		}
	})
	info := h.spawn(t)
	child := h.childOf(t, info.TaskID)
	for range 2 {
		cur := h.get(t, info.TaskID)
		h.m.OnRunFinished(ctx, child, RunOutcome{RunID: cur.RunID, Status: StatusCompleted, Text: "more"})
		if got := h.get(t, info.TaskID); got.Status != StatusWorking || got.RunID == cur.RunID {
			t.Fatalf("under the bound: %s on %s, want a new run", got.Status, got.RunID)
		}
	}
	// The third ending would chain a third run: refused.
	cur := h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, child, RunOutcome{RunID: cur.RunID, Status: StatusCompleted, Text: "more"})
	got := h.get(t, info.TaskID)
	if got.Status != StatusFailed || !strings.Contains(got.Summary, "continuation ceiling") {
		t.Fatalf("at the bound: %s %q, want failed at the ceiling", got.Status, got.Summary)
	}
	if n := len(h.launcher.all()); n != 3 {
		t.Fatalf("launches = %d, want the spawn and two continuations", n)
	}
	if fin := h.reportedFinished(); len(fin) != 1 || fin[0].Status != StatusFailed {
		t.Fatalf("finished reports = %+v, want the one failure", fin)
	}
	// A retry is a person going on: the count starts over, so the chain may
	// run the bound again before it is stopped.
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	cur = h.get(t, info.TaskID)
	h.m.OnRunFinished(ctx, child, RunOutcome{RunID: cur.RunID, Status: StatusCompleted, Text: "more"})
	if got := h.get(t, info.TaskID); got.Status != StatusWorking {
		t.Fatalf("after a retry: %s, want the chain going again", got.Status)
	}
}
