package tasks

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fail drives a task to failed the way a run does, so a test can start from the
// one state a retry is allowed to resume.
func (h *harness) fail(t *testing.T, taskID, reason string) {
	t.Helper()
	h.m.OnRunFinished(context.Background(), h.childOf(t, taskID),
		RunOutcome{RunID: h.get(t, taskID).RunID, Status: StatusFailed, Err: reason})
	if got := h.get(t, taskID).Status; got != StatusFailed {
		t.Fatalf("task is %s, want failed", got)
	}
}

// A retry keeps the task and its session, and changes only the run: that is
// what makes it a resumption rather than a second task doing the same work
// from nothing.
func TestRetry_KeepsTheTaskAndSessionAndChangesTheRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	before := h.get(t, info.TaskID)
	h.fail(t, info.TaskID, "rate limited")

	got, err := h.m.Retry(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	after := h.get(t, info.TaskID)

	if after.ID != before.ID || after.ChildSessionID != before.ChildSessionID {
		t.Errorf("retry moved the task: id %q→%q, session %q→%q",
			before.ID, after.ID, before.ChildSessionID, after.ChildSessionID)
	}
	if after.RunID == before.RunID {
		t.Error("retry reused the failed run's id")
	}
	if after.Status != StatusWorking || got.Status != StatusWorking {
		t.Errorf("status = %s (reported %s), want working", after.Status, got.Status)
	}
	if after.AttemptNo() != 2 || got.Attempt != 2 {
		t.Errorf("attempt = %d (reported %d), want 2", after.AttemptNo(), got.Attempt)
	}
	// The previous attempt's account of itself is gone: left in place it reads
	// as this attempt failing right now.
	if after.Summary != "" || after.Result != "" {
		t.Errorf("failure survived the claim: summary %q, result %q", after.Summary, after.Result)
	}

	launches := h.launcher.all()
	last := launches[len(launches)-1]
	if last.RunID != after.RunID || last.SessionID != before.ChildSessionID {
		t.Errorf("launch = run %q session %q, want run %q session %q",
			last.RunID, last.SessionID, after.RunID, before.ChildSessionID)
	}
	if last.Wake {
		t.Error("a retry is the task's own run, not a wake-up")
	}
	if string(last.Inherit) != string(before.Inherit) {
		t.Errorf("inherit = %s, want the spawn snapshot %s", last.Inherit, before.Inherit)
	}
	// The run has to be told why it woke up, or it starts the task over.
	if !strings.Contains(last.Input, "rate limited") {
		t.Errorf("retry input does not name the failure: %q", last.Input)
	}
}

// The failure reason reaches the new run even when only a summary was recorded
// — which is the case a restart sweep leaves behind, and exactly when a retry
// is most likely.
func TestRetry_PromptFallsBackToTheSummary(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	if _, err := h.store.FailOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if r := h.get(t, info.TaskID).Result; r != "" {
		t.Fatalf("precondition: FailOrphans recorded a result %q", r)
	}

	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	launches := h.launcher.all()
	if in := launches[len(launches)-1].Input; !strings.Contains(in, "the process restarted") {
		t.Errorf("retry input = %q, want the summary as the reason", in)
	}
}

// Only a failed task can be resumed. The other endings are not failures to
// recover from: a completed task has its answer, and a cancelled one was
// stopped on purpose.
func TestRetry_RefusesEveryStatusButFailed(t *testing.T) {
	ctx := context.Background()
	// Each case drives the task to the status it is named for, using only the
	// task and its child session — no *testing.T, so these stay plain drivers
	// rather than helpers.
	for _, tc := range []struct {
		status Status
		drive  func(h *harness, id, child string)
	}{
		{StatusWorking, func(*harness, string, string) {}},
		{StatusCompleted, func(h *harness, _, child string) {
			h.m.OnRunFinished(ctx, child, RunOutcome{Status: StatusCompleted, Text: "done"})
		}},
		{StatusCancelled, func(h *harness, id, _ string) {
			_, _ = h.m.Stop(ctx, id, false)
		}},
		{StatusInputRequired, func(h *harness, _, child string) {
			h.m.OnRunFinished(ctx, child, RunOutcome{Status: StatusInputRequired})
		}},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			h := newHarness(t)
			info := h.spawn(t)
			tc.drive(h, info.TaskID, h.childOf(t, info.TaskID))
			before := h.get(t, info.TaskID)
			if before.Status != tc.status {
				t.Fatalf("task is %s, want the case's own %s", before.Status, tc.status)
			}

			got, err := h.m.Retry(ctx, info.TaskID)
			var refusal ErrNotRetryable
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want ErrNotRetryable", err)
			}
			if refusal.Status != before.Status {
				t.Errorf("refusal names %s, want %s", refusal.Status, before.Status)
			}
			// The state travels with the refusal so a caller can show what the
			// task actually is, not only that it said no.
			if got == nil || got.Status != before.Status {
				t.Errorf("info = %+v, want the task's current state", got)
			}
			if after := h.get(t, info.TaskID); after.RunID != before.RunID {
				t.Error("a refused retry started a run anyway")
			}
		})
	}
}

// The attempt ceiling is what keeps a model from retrying a failure that was
// never going to succeed until it runs out of turns.
func TestRetry_StopsAtTheAttemptLimit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxAttemptsPerTask = 2 })
	info := h.spawn(t)

	h.fail(t, info.TaskID, "boom")
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatalf("second attempt refused: %v", err)
	}
	h.fail(t, info.TaskID, "boom again")

	_, err := h.m.Retry(ctx, info.TaskID)
	var limit ErrRetryLimit
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want ErrRetryLimit", err)
	}
	if limit.Limit != 2 {
		t.Errorf("limit = %d, want 2", limit.Limit)
	}
	if got := h.get(t, info.TaskID).AttemptNo(); got != 2 {
		t.Errorf("attempt = %d, want it to stay at 2", got)
	}
}

// A limit of 1 is how a host turns retrying off, and it must hold on the very
// first ask rather than after one free retry.
func TestRetry_LimitOfOneDisablesRetrying(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxAttemptsPerTask = 1 })
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	if _, err := h.m.Retry(ctx, info.TaskID); !errors.As(err, new(ErrRetryLimit)) {
		t.Fatalf("err = %v, want ErrRetryLimit", err)
	}
}

// A retry is a task coming back to life, so it queues behind the same ceiling a
// spawn does. Exempting it would make retry the way around the cap.
func TestRetry_TakesAConcurrencySlot(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxConcurrentPerParent = 2 })
	first := h.spawn(t)
	h.fail(t, first.TaskID, "boom")
	// Two live tasks fill the cap the failed one vacated.
	h.spawn(t)
	h.spawn(t)

	_, err := h.m.Retry(ctx, first.TaskID)
	if !errors.As(err, new(ErrTaskLimit)) {
		t.Fatalf("err = %v, want ErrTaskLimit", err)
	}
	if got := h.get(t, first.TaskID).Status; got != StatusFailed {
		t.Errorf("status = %s, want the task left failed", got)
	}
}

// Two retries of one task must not both start a run: the claim is a
// compare-and-set, and the loser is told no rather than launching a second run
// against the same session.
func TestRetry_ConcurrentRetriesProduceOneRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	var mu sync.Mutex
	var wins int
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := h.m.Retry(ctx, info.TaskID); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d retries won, want exactly 1", wins)
	}
	if got := h.get(t, info.TaskID).AttemptNo(); got != 2 {
		t.Errorf("attempt = %d, want 2 — one claim, one increment", got)
	}
	var runs int
	for _, r := range h.launcher.all() {
		if r.SessionID == h.childOf(t, info.TaskID) {
			runs++
		}
	}
	if runs != 2 { // the original plus one retry
		t.Errorf("%d runs on the task session, want 2", runs)
	}
}

// A retry whose run never starts must not leave the task working: nothing is
// going to advance it, and its slot would be held forever.
func TestRetry_LaunchFailurePutsTheTaskBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	h.launcher.err = errors.New("session is being deleted")
	cur, err := h.m.Retry(ctx, info.TaskID)
	if err == nil {
		t.Fatal("retry reported success with no run started")
	}
	// The state travels with the error, so the caller can report what the
	// task now is — the tool path needs it to hand the model the failure.
	if cur == nil {
		t.Fatal("no task state came back with the launch failure")
	}
	// Attempt is back at 1: the claimed run never launched, so it never
	// counted — infrastructure failures must not spend the retry ceiling.
	if cur.Status != StatusFailed || cur.Attempt != 1 {
		t.Errorf("reported %s attempt %d, want failed attempt 1 (the claim rolled back)", cur.Status, cur.Attempt)
	}

	after := h.get(t, info.TaskID)
	if after.Status != StatusFailed {
		t.Errorf("status = %s, want failed", after.Status)
	}
	if !strings.Contains(after.Summary, "retry could not start") {
		t.Errorf("summary = %q, want it to say the retry never started", after.Summary)
	}
	// The new ending is reported like any other: whether the MODEL has heard
	// is the caller's knowledge, and Retry alone (the host-API path) has told
	// it nothing.
	if got := h.reportedFinished(); len(got) == 0 || got[len(got)-1].ID != info.TaskID {
		t.Fatalf("reported = %+v, want the failed retry's ending", got)
	}
	if got := h.reportedFinished(); !strings.Contains(got[len(got)-1].Summary, "retry could not start") {
		t.Errorf("the reported ending does not carry the failure: %q", got[len(got)-1].Summary)
	}
}

// Launch failures do not spend the retry ceiling: however many times the
// infrastructure refuses to start the run, the task keeps its attempts and a
// later retry that does launch still succeeds.
func TestRetry_LaunchFailuresDoNotSpendAttempts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	h.launcher.err = errors.New("shutting down")
	for range h.m.MaxAttempts() + 1 { // more failures than the ceiling has attempts
		if _, err := h.m.Retry(ctx, info.TaskID); err == nil {
			t.Fatal("retry reported success with no run started")
		}
	}
	if got := h.get(t, info.TaskID).AttemptNo(); got != 1 {
		t.Fatalf("attempt = %d after launch failures, want 1 — none of them ran", got)
	}

	h.launcher.err = nil
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatalf("retry after the launcher recovered: %v", err)
	}
	if got := h.get(t, info.TaskID).AttemptNo(); got != 2 {
		t.Fatalf("attempt = %d, want 2 — the launched retry counts", got)
	}
}

// The wake-up debt a failed retry-launch opens follows the ModelHasResult
// line: the task_retry tool hands the model the failure in its result and
// settles the debt in hand; a retry over a host API tells only a person, so
// the model keeps its wake-up — immediately, when the parent is idle.
func TestRetry_LaunchFailureDebtFollowsTheCaller(t *testing.T) {
	ctx := context.Background()

	t.Run("the tool reports the failure and settles the debt", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.fail(t, info.TaskID, "boom")
		h.launcher.err = errors.New("nope")

		res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_retry"), "parent",
			`{"task_id":"`+info.TaskID+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Error("a retry that never started reported success")
		}
		if s := stringOf(res); !strings.Contains(s, "retry could not start") {
			t.Fatalf("the model was not told why: %q", s)
		}
		if got := h.reportedDelivered(); len(got) == 0 || got[len(got)-1].ID != info.TaskID {
			t.Errorf("delivered = %+v, want it reported as read — the model just saw the failure", got)
		}
	})

	t.Run("a host-API retry wakes an idle parent at once", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.fail(t, info.TaskID, "boom") // wake #1: the original failure
		// Only the retry's own run fails to start; the wake-up that follows
		// can go out. The hook runs on the launching goroutine, so flipping
		// err per request is ordered, not raced.
		h.launcher.beforeLaunch = func(req LaunchRequest) {
			if req.Wake {
				h.launcher.err = nil
			} else {
				h.launcher.err = errors.New("nope")
			}
		}
		if _, err := h.m.Retry(ctx, info.TaskID); err == nil {
			t.Fatal("retry reported success with no run started")
		}
		reported := h.reportedFinished()
		if len(reported) != 2 {
			t.Fatalf("%d endings reported, want 2: the original failure and the failed retry", len(reported))
		}
		if got := reported[1].Summary; !strings.Contains(got, "retry could not start") {
			t.Errorf("the reported ending does not carry the failure: %q", got)
		}
	})
}

// The waiters a failed retry leaves behind are the ones that were watching the
// task: a task_status wait must not sit out its full timeout because the
// failure happened on the retry path.
func TestRetry_LaunchFailureWakesWaiters(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")
	// Consume the first failure's debt so the wait below starts clean.
	if _, err := h.m.Status(ctx, info.TaskID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}

	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		if _, err := h.m.Status(ctx, info.TaskID, 10*time.Second); err != nil {
			t.Error(err)
		}
	}()

	h.launcher.err = errors.New("nope")
	h.fail(t, info.TaskID, "boom again")
	if _, err := h.m.Retry(ctx, info.TaskID); err == nil {
		t.Fatal("retry reported success with no run started")
	}
	<-waiting
}

// The whole point of a retry is that the next terminal state still reaches the
// parent — a task that came back to life owes its news again.
func TestRetry_NextEndingStillWakesTheParent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")
	if n := len(h.reportedFinished()); n != 1 {
		t.Fatalf("%d endings reported after the first failure, want 1", n)
	}

	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{
		RunID: h.get(t, info.TaskID).RunID, Status: StatusCompleted, Text: "finished on the second try",
	})

	wakes := h.reportedFinished()
	if len(wakes) != 2 {
		t.Fatalf("%d endings reported, want 2 — the retry's ending is news too", len(wakes))
	}
	if !strings.Contains(wakes[1].Summary, "finished on the second try") {
		t.Errorf("reported = %q, want the retried run's result", wakes[1].Summary)
	}
}

// A stop that read the task before a retry must not cancel the attempt that
// replaced it: the terminal state it would write is about a run that is already
// over, while the live one keeps executing with nothing able to stop it.
func TestRetry_StaleFinalizeCannotEndTheNewAttempt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	stale := h.get(t, info.TaskID).RunID
	h.fail(t, info.TaskID, "boom")
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}

	// The stop that was in flight when the retry landed.
	won, err := h.store.Finalize(ctx, info.TaskID, stale, StatusCancelled, "stopped", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("a finalizer naming the previous attempt won")
	}
	if got := h.get(t, info.TaskID).Status; got != StatusWorking {
		t.Errorf("status = %s, want the new attempt still working", got)
	}

	// And the new attempt's own ending lands normally.
	current := h.get(t, info.TaskID).RunID
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID),
		RunOutcome{RunID: current, Status: StatusCompleted, Text: "done"})
	if got := h.get(t, info.TaskID); got.Status != StatusCompleted || got.Result != "done" {
		t.Errorf("task = %s/%q, want completed/done", got.Status, got.Result)
	}
}

func TestStop_ChasesOneRetry(t *testing.T) {
	ctx := context.Background()
	var stopper func(runID string)
	// A host that answers the way the real one does: a run it has already seen
	// finish is reported as over, not as cancelled. That is precisely what a
	// stop hears about the attempt a retry replaced — the previous run IS
	// finished, which is what let the retry happen — so a fake that always
	// says "cancelled" would let this test pass on a Manager that never
	// chases.
	finished := map[string]bool{}
	h := newHarness(t, func(c *Config) {
		inner := c.Stopper
		c.Stopper = Stopper(func(ctx context.Context, runID string, graceful bool) (StopOutcome, error) {
			if stopper != nil {
				stopper(runID)
			}
			if finished[runID] {
				return StopAlreadyFinished, nil
			}
			return inner(ctx, runID, graceful)
		})
	})
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	current := h.get(t, info.TaskID).RunID

	// A retry lands between the stop's read and its claim, exactly once. The
	// attempt the stop was aiming at is finished by then — which is what the
	// host will tell it.
	var once sync.Once
	stopper = func(string) {
		once.Do(func() {
			won, err := h.store.Finalize(ctx, info.TaskID, current, StatusFailed, "boom again", "boom again", nil)
			if err != nil || !won {
				t.Errorf("staging the interleaved failure: won=%v err=%v", won, err)
			}
			finished[current] = true
			if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
				t.Errorf("staging the interleaved retry: %v", err)
			}
		})
	}

	got, err := h.m.Stop(ctx, info.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled — the stop chased the new attempt", got.Status)
	}
	if final := h.get(t, info.TaskID); final.Status != StatusCancelled {
		t.Errorf("stored status = %s, want cancelled", final.Status)
	}
}

// Starting a run is two steps — claim the row, then tell the host — and a stop
// that lands between them cancels a run the host has never heard of: its
// Stopper call reaches nothing and the launch goes ahead. Without the settle,
// the row reads cancelled while that run executes, unstoppable, its own outcome
// unrecordable — and the retry reports success.
func TestRetry_StopInsideTheLaunchWindow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	var stopped *Info
	h.launcher.beforeLaunch = func(LaunchRequest) {
		h.launcher.beforeLaunch = nil // the stop's own launches must not recurse
		var err error
		if stopped, err = h.m.Stop(ctx, info.TaskID, false); err != nil {
			t.Errorf("staging the interleaved stop: %v", err)
		}
	}

	got, err := h.m.Retry(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped == nil || stopped.Status != StatusCancelled {
		t.Fatalf("the staged stop did not cancel: %+v", stopped)
	}
	// The retry reports what the task IS, not the working state it claimed.
	if got.Status != StatusCancelled {
		t.Errorf("retry reported %s, want the cancelled state it lost to", got.Status)
	}
	if row := h.get(t, info.TaskID); row.Status != StatusCancelled {
		t.Errorf("row = %s, want cancelled", row.Status)
	}

	// And the run that was started anyway is stopped, rather than left running
	// for a task that is over.
	launched := h.launcher.all()
	newRun := launched[len(launched)-1].RunID
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Contains(h.stopped, newRun) {
		t.Errorf("run %s was launched for a cancelled task and never stopped (stopped: %v)", newRun, h.stopped)
	}
}

// The same window on the spawn path: a task row is visible to a teardown from
// the moment it exists, and its run is not.
func TestSpawn_TeardownInsideTheLaunchWindow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.launcher.beforeLaunch = func(LaunchRequest) {
		h.launcher.beforeLaunch = nil
		if err := h.m.StopTree(ctx, "parent"); err != nil {
			t.Errorf("staging the interleaved teardown: %v", err)
		}
	}

	info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusCancelled {
		t.Errorf("spawn reported %s, want the cancelled state the teardown recorded", info.Status)
	}
	launched := h.launcher.all()
	newRun := launched[len(launched)-1].RunID
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Contains(h.stopped, newRun) {
		t.Errorf("run %s was launched into a torn-down session and never stopped (stopped: %v)", newRun, h.stopped)
	}
}

// unknownRunStopper is a host that answers honestly about a run it has never
// heard of — which is what every host is during the window between a task
// claiming its run and the launch registering it.
func unknownRunStopper(h *harness, known map[string]bool) Stopper {
	return func(_ context.Context, runID string, _ bool) (StopOutcome, error) {
		h.mu.Lock()
		h.stopped = append(h.stopped, runID)
		h.mu.Unlock()
		if !known[runID] {
			return StopUnknownRun, nil
		}
		return StopCancelled, nil
	}
}

// A graceful stop may only leave the ending to the run when there IS a run to
// leave it to. A host that has never heard of it has nothing to report but
// success, and reading that as "it will wind itself up" tells the caller the
// task was stopped while it runs to completion with nobody recording anything.
func TestStop_GracefulInsideTheLaunchWindow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	known := map[string]bool{}
	h.m.cfg.Stopper = unknownRunStopper(h, known)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	var stopped *Info
	h.launcher.beforeLaunch = func(LaunchRequest) {
		h.launcher.beforeLaunch = nil
		var err error
		if stopped, err = h.m.Stop(ctx, info.TaskID, true); err != nil {
			t.Errorf("staging the graceful stop: %v", err)
		}
	}

	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	if stopped == nil || stopped.Status != StatusCancelled {
		t.Fatalf("graceful stop reported %+v, want the cancellation it had to record itself", stopped)
	}
	if row := h.get(t, info.TaskID); row.Status != StatusCancelled {
		t.Errorf("row = %s, want cancelled — nobody else was going to record it", row.Status)
	}
}

// A run that finishes before the launch call returns is ordinary — a
// pre-flight failure is nearly instant — and it leaves the same row a stop in
// the window does: terminal, on this run. Cancelling then would cancel
// something already over, and a host that keeps finished runs would rewrite
// the outcome its clients just saw.
func TestRetry_RunFinishingInsideTheLaunchWindowIsNotCancelled(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	h.mu.Lock()
	before := len(h.stopped)
	h.mu.Unlock()
	h.launcher.beforeLaunch = func(req LaunchRequest) {
		h.launcher.beforeLaunch = nil
		h.m.OnRunFinished(ctx, req.SessionID, RunOutcome{
			RunID: req.RunID, Status: StatusCompleted, Text: "done fast",
		})
	}

	got, err := h.m.Retry(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("retry reported %s, want the completed state it settled on", got.Status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.stopped) != before {
		t.Errorf("a run that completed normally was told to cancel: %v", h.stopped[before:])
	}
}

// When a task finishes that fast, its result is in the tool output the model
// is about to read — so the wake-up owes nothing. Consuming belongs to the
// MODEL path: a person reading the same result over REST has told the model
// nothing, and its wake-up must survive.
func TestTools_FastFinishReportsDeliveryOnlyForTheModel(t *testing.T) {
	ctx := context.Background()
	finishOnLaunch := func(h *harness) {
		h.launcher.beforeLaunch = func(req LaunchRequest) {
			h.launcher.beforeLaunch = nil
			h.m.OnRunFinished(ctx, req.SessionID, RunOutcome{
				RunID: req.RunID, Status: StatusCompleted, Text: "done fast",
			})
		}
	}

	t.Run("model path consumes", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.fail(t, info.TaskID, "boom")
		finishOnLaunch(h)

		res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_retry"), "parent",
			`{"task_id":"`+info.TaskID+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stringOf(res), "done fast") {
			t.Fatalf("the model was not handed the result: %q", stringOf(res))
		}
		if got := h.reportedDelivered(); len(got) == 0 || got[len(got)-1].ID != info.TaskID {
			t.Errorf("delivered = %+v, want it reported as read — the model already has it", got)
		}
	})

	t.Run("host path keeps the debt", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.fail(t, info.TaskID, "boom")
		finishOnLaunch(h)

		// Retry called directly, as a REST endpoint does.
		if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
			t.Fatal(err)
		}
		if got := h.reportedDelivered(); len(got) != 0 {
			t.Errorf("delivered = %+v, want none — the model has heard nothing", got)
		}
	})
}

// The wake-up is cancelled only for the result the model is genuinely handed.
// The two ways that can go wrong are opposite, and both leave news undelivered:
// consuming for a task the model was told is still running swallows a result
// that landed afterwards, and consuming on the row alone cancels a debt that by
// then belongs to a different attempt.
func TestModelHasResult_CancelsOnlyWhatTheModelWasHanded(t *testing.T) {
	ctx := context.Background()

	// The row finished after the answer was decided: the model holds
	// "working", so the wake-up is the only way this result ever arrives.
	t.Run("a result the model was not shown still owes a wake-up", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{
			RunID: h.get(t, info.TaskID).RunID, Status: StatusCompleted, Text: "landed late",
		})

		// info is what the tool is about to return — decided before the finish.
		h.m.ModelHasResult(ctx, info)
		if got := h.reportedDelivered(); len(got) != 0 {
			t.Errorf("delivered = %+v, want none — the model was told %q", got, info.Status)
		}
	})

	t.Run("the result the model reads cancels its own wake-up", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{
			RunID: h.get(t, info.TaskID).RunID, Status: StatusCompleted, Text: "done",
		})

		// What Status hands the model: the finished task itself.
		finished, err := h.m.Status(ctx, info.TaskID, 0)
		if err != nil {
			t.Fatal(err)
		}
		h.m.ModelHasResult(ctx, finished)
		if got := h.reportedDelivered(); len(got) == 0 || got[len(got)-1].ID != info.TaskID {
			t.Errorf("delivered = %+v, want the task the model was handed", got)
		}
	})

	// A retry between the answer and the write moves the debt to an attempt
	// nobody has been told about.
	t.Run("a later attempt's wake-up is not this result's to cancel", func(t *testing.T) {
		h := newHarness(t)
		info := h.spawn(t)
		h.fail(t, info.TaskID, "boom")
		stale := infoFrom(h.get(t, info.TaskID), "") // terminal, attempt 1

		if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
			t.Fatal(err)
		}
		h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{
			RunID: h.get(t, info.TaskID).RunID, Status: StatusFailed, Err: "boom again",
		})

		before := len(h.reportedDelivered())
		h.m.ModelHasResult(ctx, stale)
		if got := h.reportedDelivered(); len(got) != before {
			t.Errorf("delivered = %+v, want the second attempt's news untouched", got)
		}
	})
}

// A stop that arrives after the run ended on its own must not overwrite the
// outcome: the host marks a run finished before its report reaches the row, so
// this window is ordinary — and recording a cancellation there loses a
// completion, or a failure along with the retry it had earned. The stop waits
// out the window rather than racing it.
func TestStop_AfterTheRunAlreadyFinishedKeepsTheOutcome(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	child := h.childOf(t, info.TaskID)
	runID := h.get(t, info.TaskID).RunID

	// The host: this run is over, its report is on its way — and here it
	// comes, from the run's own goroutine, while the stop is waiting.
	landed := make(chan struct{})
	h.m.cfg.Stopper = Stopper(func(_ context.Context, rid string, _ bool) (StopOutcome, error) {
		h.mu.Lock()
		first := len(h.stopped) == 0
		h.stopped = append(h.stopped, rid)
		h.mu.Unlock()
		if first {
			go func() {
				defer close(landed)
				h.m.OnRunFinished(context.Background(), child, RunOutcome{
					RunID: runID, Status: StatusFailed, Err: "the real failure",
				})
			}()
		}
		return StopAlreadyFinished, nil
	})

	stopped, err := h.m.Stop(ctx, info.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	<-landed
	if stopped.Status != StatusFailed {
		t.Errorf("the stop answered %s, want the real ending it waited for", stopped.Status)
	}
	after := h.get(t, info.TaskID)
	if after.Status != StatusFailed {
		t.Errorf("status = %s, want failed — a stop overwrote a real outcome", after.Status)
	}
	// And a failure keeps the retry it earned.
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Errorf("the task lost its retry to a stop that cancelled nothing: %v", err)
	}
}

// The other half of the same window: an outcome that was LOST rather than late
// must not leave the task un-stoppable. The host keeps answering "that run is
// over" and nothing ever reaches the row — which is what a host whose store
// refused the write leaves behind — so the stop records the ending itself
// instead of reporting a task that will never end as still working.
func TestStop_AnOutcomeThatNeverLandsDoesNotWedgeTheTask(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	h.m.cfg.Stopper = Stopper(func(_ context.Context, rid string, _ bool) (StopOutcome, error) {
		h.mu.Lock()
		h.stopped = append(h.stopped, rid)
		h.mu.Unlock()
		return StopAlreadyFinished, nil
	})

	stopped, err := h.m.Stop(ctx, info.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != StatusCancelled {
		t.Errorf("the stop answered %s, want cancelled — a stop that reports the task as still\n"+
			"working leaves a dead task live in the UI, with a Stop button that does nothing", stopped.Status)
	}
	if got := h.get(t, info.TaskID).Status; got != StatusCancelled {
		t.Errorf("status = %s, want cancelled", got)
	}
}

// Retryable is about the task's own state. Capacity is deliberately not part
// of it: the parent's ceiling can change between an offer being rendered and
// someone taking it, so a precomputed answer would be wrong as often as right.
func TestRetryable_IsAboutTheTaskNotTheParentsCapacity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxConcurrentPerParent = 2 })
	first := h.spawn(t)
	h.fail(t, first.TaskID, "boom")
	h.spawn(t)
	h.spawn(t) // the parent is now full

	if _, err := h.m.Retry(ctx, first.TaskID); !errors.As(err, new(ErrTaskLimit)) {
		t.Fatalf("err = %v, want the capacity refusal to arrive at call time", err)
	}
	if h.m.MaxAttempts() != DefaultMaxAttemptsPerTask {
		t.Errorf("MaxAttempts = %d, want the configured ceiling", h.m.MaxAttempts())
	}
}
