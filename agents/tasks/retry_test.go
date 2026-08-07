package tasks

import (
	"context"
	"errors"
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
	// Nothing is owed while a task is running again.
	if after.NotifyState != NotifyNone {
		t.Errorf("notify state = %q, want cleared", after.NotifyState)
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
	for _, tc := range []struct {
		name  string
		drive func(*harness, *testing.T, string)
	}{
		{"working", func(*harness, *testing.T, string) {}},
		{"completed", func(h *harness, t *testing.T, id string) {
			h.m.OnRunFinished(ctx, h.childOf(t, id), RunOutcome{Status: StatusCompleted, Text: "done"})
		}},
		{"cancelled", func(h *harness, t *testing.T, id string) {
			if _, err := h.m.Stop(ctx, id, false); err != nil {
				t.Fatal(err)
			}
		}},
		{"input_required", func(h *harness, t *testing.T, id string) {
			h.m.OnRunFinished(ctx, h.childOf(t, id), RunOutcome{Status: StatusInputRequired})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			info := h.spawn(t)
			tc.drive(h, t, info.TaskID)
			before := h.get(t, info.TaskID)

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
	if _, err := h.m.Retry(ctx, info.TaskID); err == nil {
		t.Fatal("retry reported success with no run started")
	}

	after := h.get(t, info.TaskID)
	if after.Status != StatusFailed {
		t.Errorf("status = %s, want failed", after.Status)
	}
	if !strings.Contains(after.Summary, "retry could not start") {
		t.Errorf("summary = %q, want it to say the retry never started", after.Summary)
	}
	// The caller was told to its face, so there is no news to wake anyone
	// with: a drain that could deliver finds nothing owed.
	if after.NotifyState != NotifyConsumed {
		t.Errorf("notify state = %q, want consumed", after.NotifyState)
	}
	h.launcher.err = nil
	before := len(h.launcher.wakes())
	h.m.DrainPending(ctx, "parent")
	if len(h.launcher.wakes()) != before {
		t.Error("a failed retry left a wake-up owed")
	}
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
	// The first failure's wake-up, delivered.
	if n := len(h.launcher.wakes()); n != 1 {
		t.Fatalf("%d wake-ups after the first failure, want 1", n)
	}

	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{
		RunID: h.get(t, info.TaskID).RunID, Status: StatusCompleted, Text: "finished on the second try",
	})

	wakes := h.launcher.wakes()
	if len(wakes) != 2 {
		t.Fatalf("%d wake-ups, want 2 — the retry's ending owes one too", len(wakes))
	}
	if !strings.Contains(wakes[1].Input, "finished on the second try") {
		t.Errorf("wake-up = %q, want the retried run's result", wakes[1].Input)
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
	won, err := h.store.Finalize(ctx, info.TaskID, stale, StatusCancelled, "stopped", "")
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

// The same rule for the notify writes: a consume decided against the previous
// attempt must not swallow the debt of the one that replaced it, or the
// parent is never told how the task actually ended.
func TestRetry_StaleNotifyWriteCannotSwallowTheNewDebt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	stale := h.get(t, info.TaskID).RunID
	h.fail(t, info.TaskID, "boom")
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	current := h.get(t, info.TaskID).RunID
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID),
		RunOutcome{RunID: current, Status: StatusCompleted, Text: "done"})

	// The late arrivals, both naming the attempt that is gone.
	if err := h.store.ConsumeNotify(ctx, info.TaskID, stale); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkNotifyDelivered(ctx, info.TaskID, stale); err != nil {
		t.Fatal(err)
	}

	// DrainPending already delivered this one, which is the state that matters:
	// what must NOT have happened is the stale consume marking it consumed.
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyDelivered {
		t.Errorf("notify state = %q, want delivered by the retry's own drain", got)
	}
}

// A stop of a task that was retried underneath it chases the new attempt: the
// caller asked to stop the TASK, and reporting "still running" would be a stop
// that did nothing.
func TestStop_ChasesOneRetry(t *testing.T) {
	ctx := context.Background()
	var stopper func(runID string)
	h := newHarness(t, func(c *Config) {
		inner := c.Stopper
		c.Stopper = StopperFunc(func(ctx context.Context, runID string, graceful bool) error {
			if stopper != nil {
				stopper(runID)
			}
			return inner.Stop(ctx, runID, graceful)
		})
	})
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")
	if _, err := h.m.Retry(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	current := h.get(t, info.TaskID).RunID

	// A retry lands between the stop's read and its claim, exactly once.
	var once sync.Once
	stopper = func(string) {
		once.Do(func() {
			won, err := h.store.Finalize(ctx, info.TaskID, current, StatusFailed, "boom again", "boom again")
			if err != nil || !won {
				t.Errorf("staging the interleaved failure: won=%v err=%v", won, err)
			}
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

// A task that does not exist is a different answer from one that cannot be
// claimed, and both shipped stores must agree — a caller written against one
// backend has to be right on the other.
func TestRetryClaim_MissingTaskIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	won, err := s.RetryClaim(ctx, "nope", "run2", 3)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if won {
		t.Error("claimed a task that does not exist")
	}
}

// A store-level guard, not only a Manager-level one: the ceiling has to hold
// when two processes ask at once, and only the row can arbitrate that.
func TestRetryClaim_EnforcesTheCeilingItself(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	task := &Task{ID: "t1", RunID: "run1", ParentSessionID: "p", ChildSessionID: "c",
		Attempt: 3, Status: StatusFailed}
	if err := s.Create(ctx, task); err != nil {
		t.Fatal(err)
	}

	won, err := s.RetryClaim(ctx, "t1", "run2", 3)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("claimed past the ceiling")
	}
	// Zero means no ceiling: a host that wants unlimited retries says so.
	if won, err = s.RetryClaim(ctx, "t1", "run2", 0); err != nil || !won {
		t.Errorf("unlimited claim: won=%v err=%v", won, err)
	}
}
