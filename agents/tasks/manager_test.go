package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// ── §2.1 lifecycle and identity ─────────────────────────────────────────────

// #1: a task is the durable entity, a run is one attempt at it. Collapsing them
// would make a retry impossible to express without inventing a second task.
func TestTask_IdentityIsSeparateFromExecution(t *testing.T) {
	h := newHarness(t)
	info := h.spawn(t)
	task := h.get(t, info.TaskID)
	if task.RunID == "" || task.RunID == task.ID {
		t.Errorf("run id %q vs task id %q — they must be distinct", task.RunID, task.ID)
	}
}

// #2: each task gets its own hidden session, so a task transcript never buries
// the conversations the user actually started.
func TestTask_ChildSessionIsHidden(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	visible, err := h.repo.List(ctx, session.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range visible {
		if md.ID == h.childOf(t, info.TaskID) {
			t.Error("the task session appears in the default listing")
		}
	}
	all, err := h.repo.List(ctx, session.ListOptions{IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, md := range all {
		if md.ID == h.childOf(t, info.TaskID) {
			found = true
			if !strings.Contains(md.Title, "job") {
				t.Errorf("title = %q, want the label in it", md.Title)
			}
		}
	}
	if !found {
		t.Error("the task session is missing even with IncludeHidden")
	}
}

// #3: the wake-up runs under the configuration the SPAWNING run had. Resolving
// it fresh would use whatever the parent is configured with now — possibly a
// different agent entirely.
func TestTask_WakeUsesTheSnapshottedConfig(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	wakes := h.launcher.wakes()
	if len(wakes) != 1 {
		t.Fatalf("%d wake-ups, want 1", len(wakes))
	}
	var got map[string]string
	if err := json.Unmarshal(wakes[0].Inherit, &got); err != nil {
		t.Fatal(err)
	}
	if got["agent"] != "worker" {
		t.Errorf("wake inherited %v, want the spawning run's config", got)
	}
}

// #4: a task that can spawn tasks can spawn them forever.
func TestTask_DepthIsBounded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	parent := h.spawn(t)
	childSession := h.childOf(t, parent.TaskID)

	_, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: childSession, AgentName: "worker", Input: "again"})
	var depth ErrDepthLimit
	if !errors.As(err, &depth) {
		t.Fatalf("err = %v, want ErrDepthLimit", err)
	}
}

// A raised limit allows one more hop — recursion is a real use case, it just
// must not be the default.
func TestTask_DepthLimitIsConfigurable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxDepth = 2 })
	parent := h.spawn(t)

	child, err := h.m.Spawn(ctx, SpawnRequest{
		ParentSessionID: h.childOf(t, parent.TaskID), AgentName: "worker", Input: "again",
	})
	if err != nil {
		t.Fatalf("depth 2 was refused: %v", err)
	}
	if got := h.get(t, child.TaskID).Depth; got != 2 {
		t.Errorf("depth = %d, want 2", got)
	}
	_, err = h.m.Spawn(ctx, SpawnRequest{
		ParentSessionID: h.childOf(t, child.TaskID), AgentName: "worker", Input: "deeper",
	})
	if !errors.As(err, &ErrDepthLimit{}) {
		t.Errorf("err = %v, want the raised limit to still bound", err)
	}
}

// #5: a model told it can delegate will delegate.
func TestTask_ConcurrencyIsCapped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxConcurrentPerParent = 2 })
	h.spawn(t)
	h.spawn(t)

	_, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "third"})
	var limit ErrTaskLimit
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want ErrTaskLimit", err)
	}
	if limit.Limit != 2 {
		t.Errorf("limit = %d, want 2", limit.Limit)
	}
	// A finished task frees its slot.
	live, _ := h.store.ListNonTerminal(ctx, "parent")
	if _, err := h.m.Stop(ctx, live[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "third"}); err != nil {
		t.Errorf("a freed slot was not reused: %v", err)
	}
}

// ── §2.2 notification and wake-up ───────────────────────────────────────────

// #6: the debt is persisted, because a task that finished while the parent was
// busy and a process that died before delivering look the same from the
// parent's side — it was never told.
func TestNotify_DebtIsRecordedOnFinalize(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.canWake = false // nobody can be woken; the debt must survive
	info := h.spawn(t)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	task := h.get(t, info.TaskID)
	if task.NotifyState != NotifyPending {
		t.Errorf("notify state = %q, want pending", task.NotifyState)
	}
	// And it is delivered once waking becomes possible.
	h.canWake = true
	h.m.DrainPending(ctx, "parent")
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyDelivered {
		t.Errorf("notify state = %q, want delivered", got)
	}
}

// #7: two finalizers race routinely — a run completing while a stop is in
// flight. Without the CAS both write, and a terminal state gets overwritten.
func TestNotify_FinalizeIsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	if _, err := h.m.Stop(ctx, info.TaskID, false); err != nil {
		t.Fatal(err)
	}
	// The run's own completion arrives afterwards and must not overwrite.
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "too late"})

	task := h.get(t, info.TaskID)
	if task.Status != StatusCancelled {
		t.Errorf("status = %q, want the cancellation to stand", task.Status)
	}
	if task.Result == "too late" {
		t.Error("the losing finalizer overwrote the result")
	}
}

// The same, under real concurrency.
func TestNotify_ConcurrentFinalizersProduceOneWinner(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	runID := h.get(t, info.TaskID).RunID

	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := StatusCompleted
			if i%2 == 0 {
				st = StatusFailed
			}
			won, err := h.store.Finalize(ctx, info.TaskID, runID, st, "s", "r")
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
}

// #8: four guards, and each one was a bug. A guard that cannot answer must
// refuse — "cannot prove it is safe" is not permission.
func TestNotify_GuardRefusalKeepsTheDebt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.canWake = false
	info := h.spawn(t)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	if len(h.launcher.wakes()) != 0 {
		t.Error("a refused guard still woke the parent")
	}
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyPending {
		t.Errorf("notify state = %q, want the debt kept", got)
	}
}

func TestNotify_AllGuardsRequiresEveryOne(t *testing.T) {
	ctx := context.Background()
	yes := WakeGuard(func(context.Context, string) bool { return true })
	no := WakeGuard(func(context.Context, string) bool { return false })

	if !AllGuards(yes, yes)(ctx, "s") {
		t.Error("all-yes refused")
	}
	if AllGuards(yes, no)(ctx, "s") {
		t.Error("one refusal was not enough to refuse")
	}
	// A guard that was supposed to be there and is not cannot read as
	// permission.
	if AllGuards(yes, nil)(ctx, "s") {
		t.Error("a nil guard counted as permission")
	}
	if AllGuards()(ctx, "s") != true {
		t.Error("an empty set should pass; the refusals are the guards")
	}
}

// A Manager with no guard at all never wakes: it cannot tell whether waking is
// safe, and must not guess.
func TestNotify_NoGuardNeverWakes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.Guard = nil })
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})
	if len(h.launcher.wakes()) != 0 {
		t.Error("a Manager without a guard woke the parent")
	}
}

// #9: a cancellation is the user's own doing, the UI already shows it, and a
// wake-up run would only restate it.
func TestNotify_CancellationDoesNotWake(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	if _, err := h.m.Stop(ctx, info.TaskID, false); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyConsumed {
		t.Errorf("notify state = %q, want consumed", got)
	}
	h.m.DrainPending(ctx, "parent")
	if len(h.launcher.wakes()) != 0 {
		t.Error("a cancelled task woke the parent")
	}
}

// #10: losing the race for the session keeps the debt, so the winner re-drains
// at its own boundary.
func TestNotify_LosingTheLaunchKeepsTheDebt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.canWake = false
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	// Waking is allowed now, but the launcher refuses (a user run won).
	h.canWake = true
	h.launcher.err = errors.New("session busy")
	h.m.DrainPending(ctx, "parent")
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyPending {
		t.Errorf("notify state = %q, want the debt kept after a lost race", got)
	}

	// The winner's boundary re-drains it.
	h.launcher.err = nil
	h.m.DrainPending(ctx, "parent")
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyDelivered {
		t.Errorf("notify state = %q, want delivered on the retry", got)
	}
}

// A parent's own run boundary pays the debts it accumulated while busy.
func TestNotify_ParentRunBoundaryDrains(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.canWake = false
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	h.canWake = true
	// A run finishing on a session that is NOT a task session drains it.
	h.m.OnRunFinished(ctx, "parent", RunOutcome{Status: StatusCompleted, Text: "user turn done"})
	if len(h.launcher.wakes()) != 1 {
		t.Errorf("%d wake-ups, want the parent's boundary to have drained", len(h.launcher.wakes()))
	}
}

// #11: a task run does not survive a restart, so a row left at working can
// never progress on its own.
func TestNotify_RestartFailsOrphansAndDrains(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	if err := h.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	task := h.get(t, info.TaskID)
	if task.Status != StatusFailed {
		t.Errorf("status = %q, want failed", task.Status)
	}
	if !strings.Contains(task.Summary, "restart") {
		t.Errorf("summary = %q, want it to say why", task.Summary)
	}
	if len(h.launcher.wakes()) != 1 {
		t.Errorf("%d wake-ups, want the restart's news delivered", len(h.launcher.wakes()))
	}
}

// A task paused on an approval is NOT an orphan: the approval persists and
// resumes the run.
func TestNotify_RecoverKeepsInputRequired(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusInputRequired})

	if err := h.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, info.TaskID).Status; got != StatusInputRequired {
		t.Errorf("status = %q, want input_required preserved across a restart", got)
	}
}

// #12: a task returning ten thousand words must not paste them into the
// parent's context to say it is done.
func TestNotify_CarriesASummaryNotTheResult(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.SummaryLimit = 20 })
	info := h.spawn(t)
	long := strings.Repeat("x", 500)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: long})

	task := h.get(t, info.TaskID)
	if len([]rune(task.Summary)) != 20 {
		t.Errorf("summary is %d runes, want 20", len([]rune(task.Summary)))
	}
	if task.Result != long {
		t.Error("the full result was not kept")
	}
	wakes := h.launcher.wakes()
	if len(wakes) != 1 {
		t.Fatalf("%d wake-ups", len(wakes))
	}
	if len(wakes[0].Input) > 300 {
		t.Errorf("the notification is %d bytes; it should carry the summary", len(wakes[0].Input))
	}
	if !strings.Contains(wakes[0].Input, "truncated") {
		t.Error("the notification does not say where the rest is")
	}
}

// One wake-up carries every pending task: a dozen finishing together must not
// mean a dozen runs each restating the others' news.
func TestNotify_BatchesEveryPendingTask(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.canWake = false
	a := h.spawn(t)
	b := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, a.TaskID), RunOutcome{Status: StatusCompleted, Text: "first"})
	h.m.OnRunFinished(ctx, h.childOf(t, b.TaskID), RunOutcome{Status: StatusFailed, Err: "boom"})

	h.canWake = true
	h.m.DrainPending(ctx, "parent")
	wakes := h.launcher.wakes()
	if len(wakes) != 1 {
		t.Fatalf("%d wake-ups, want 1 carrying both", len(wakes))
	}
	input := wakes[0].Input
	if !strings.Contains(input, "("+a.TaskID+")") || !strings.Contains(input, "("+b.TaskID+")") {
		t.Fatalf("wake input %q, want both task ids", input)
	}
}

// ── §2.3 consistency and cleanup ────────────────────────────────────────────

// #13: a spawn that fails halfway must not leave a ghost child session the user
// can open and nothing owns.
func TestSpawn_RollsBackAGhostSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.launcher.err = errors.New("no capacity")

	_, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "go"})
	if err == nil {
		t.Fatal("expected the spawn to fail")
	}
	if len(h.repo.deletes()) != 1 {
		t.Errorf("%d sessions deleted, want the child cleaned up", len(h.repo.deletes()))
	}
	live, _ := h.store.ListNonTerminal(ctx, "parent")
	if len(live) != 0 {
		t.Errorf("%d task rows left behind", len(live))
	}
}

// The rollback runs on a detached context, because Spawn is called from inside
// the parent run: a parent cancellation racing the spawn would otherwise kill
// the cleanup halfway.
func TestSpawn_RollbackSurvivesAParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newHarness(t)
	h.launcher.err = errors.New("no capacity")

	// The parent is cancelled at the moment the launch fails.
	h.m.cfg.Launcher = Launcher(func(context.Context, LaunchRequest) error {
		cancel()
		return errors.New("no capacity")
	})

	if _, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "go"}); err == nil {
		t.Fatal("expected the spawn to fail")
	}
	if len(h.repo.deletes()) != 1 {
		t.Errorf("%d sessions deleted; a cancelled parent left a ghost", len(h.repo.deletes()))
	}
}

// #15: a teardown must stop the tasks before the cascade, or a task finishing
// mid-delete drains a notification that starts a run outliving it.
func TestStopTree_CancelsEveryLiveTask(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a := h.spawn(t)
	b := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, b.TaskID), RunOutcome{Status: StatusCompleted, Text: "already done"})

	if err := h.m.StopTree(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, a.TaskID).Status; got != StatusCancelled {
		t.Errorf("live task status = %q, want cancelled", got)
	}
	// A task that had already finished is left alone rather than reported as
	// an error.
	if got := h.get(t, b.TaskID).Status; got != StatusCompleted {
		t.Errorf("finished task status = %q, want it untouched", got)
	}
}

// ── status, stop, and the state machine ─────────────────────────────────────

// The wait trades one blocked goroutine for the model's polling loop.
func TestStatus_WaitsForCompletion(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	go func() {
		time.Sleep(20 * time.Millisecond)
		h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "finished"})
	}()

	start := time.Now()
	got, err := h.m.Status(ctx, info.TaskID, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if time.Since(start) > time.Second {
		t.Error("the wait did not return promptly when the task finished")
	}
}

// Reaching a terminal status through task_status consumes the debt: the model
// has the result, and waking it later to repeat the news would burn a turn.
func TestStatus_TerminalReadConsumesTheDebt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.canWake = false
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	if _, err := h.m.Status(ctx, info.TaskID, 0); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, info.TaskID).NotifyState; got != NotifyConsumed {
		t.Errorf("notify state = %q, want consumed", got)
	}
	h.canWake = true
	h.m.DrainPending(ctx, "parent")
	if len(h.launcher.wakes()) != 0 {
		t.Error("a consumed task still woke the parent")
	}
}

// The wait is bounded, so a stuck task returns control to the model.
func TestStatus_WaitIsBounded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.MaxStatusWait = 30 * time.Millisecond })
	info := h.spawn(t)

	start := time.Now()
	got, err := h.m.Status(ctx, info.TaskID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusWorking {
		t.Errorf("status = %q, want working", got.Status)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %v; the configured bound was not applied", elapsed)
	}
}

// A clean finish under a graceful stop IS a cancellation. Recording it as a
// completion would tell the user their stop did nothing.
func TestOnRunFinished_GracefulStopIsACancellation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{
		Status: StatusCompleted, GracefulStop: true,
	})
	task := h.get(t, info.TaskID)
	if task.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled", task.Status)
	}
	if task.Summary == "" {
		t.Error("a graceful stop left no explanation")
	}
}

// A failed run's reason travels on the outcome: without it the task would only
// ever say "failed" with no why.
func TestOnRunFinished_FailureCarriesTheReason(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusFailed, Err: "model refused"})
	if got := h.get(t, info.TaskID).Summary; !strings.Contains(got, "model refused") {
		t.Errorf("summary = %q, want the failure reason", got)
	}
}

// input_required is not terminal: delivering a notification for it would
// announce something that has not happened.
func TestOnRunFinished_InputRequiredIsNotTerminal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusInputRequired})
	task := h.get(t, info.TaskID)
	if task.Status != StatusInputRequired {
		t.Errorf("status = %q", task.Status)
	}
	if task.NotifyState != NotifyNone {
		t.Errorf("notify state = %q, want no debt for a paused task", task.NotifyState)
	}
	if len(h.launcher.wakes()) != 0 {
		t.Error("a paused task woke the parent")
	}

	// The resumed run lands back here with a final status.
	if ok, err := h.store.ReclaimWorking(ctx, info.TaskID, h.get(t, info.TaskID).RunID); err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "approved and done"})
	if got := h.get(t, info.TaskID).Status; got != StatusCompleted {
		t.Errorf("status after resume = %q, want completed", got)
	}
}

// #16: a task's state changes long after the spawning turn ended, which is the
// whole difficulty. The host is told so it can update the card.
func TestOnTaskUpdate_ReportsStateChanges(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []Status
	h := newHarness(t, func(c *Config) {
		c.OnTaskUpdate = func(_ context.Context, task *Task) {
			mu.Lock()
			seen = append(seen, task.Status)
			mu.Unlock()
		}
	})
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != StatusWorking || seen[len(seen)-1] != StatusCompleted {
		t.Errorf("updates = %v, want working then completed", seen)
	}
}

// MetaFor is how a host decides not to give a task run the task tools.
func TestMetaFor_IdentifiesATaskSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	meta, ok, err := h.m.MetaFor(ctx, h.childOf(t, info.TaskID))
	if err != nil {
		t.Fatalf("MetaFor: %v", err)
	}
	if !ok {
		t.Fatal("a task session was not recognized")
	}
	if meta.TaskID != info.TaskID || meta.ParentSessionID != "parent" {
		t.Errorf("meta = %+v", meta)
	}
	switch _, ok, err := h.m.MetaFor(ctx, "parent"); {
	case err != nil:
		t.Errorf("an ordinary session reported an error: %v", err)
	case ok:
		t.Error("an ordinary session was reported as a task session")
	}
}

func TestStop_AlreadyFinalIsReportedNotRetried(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	got, err := h.m.Stop(ctx, info.TaskID, false)
	var final ErrAlreadyFinal
	if !errors.As(err, &final) {
		t.Fatalf("err = %v, want ErrAlreadyFinal", err)
	}
	if got == nil || got.Status != StatusCompleted {
		t.Errorf("info = %+v, want the terminal state reported", got)
	}
}

// A graceful stop lets the run finish its turn and report through
// OnRunFinished; finalizing here would race it.
func TestStop_GracefulDefersToTheRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)

	if _, err := h.m.Stop(ctx, info.TaskID, true); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, info.TaskID).Status; got != StatusWorking {
		t.Errorf("status = %q, want the run to still own the transition", got)
	}
	h.mu.Lock()
	stopped := len(h.stopped)
	h.mu.Unlock()
	if stopped != 1 {
		t.Errorf("%d stops issued, want 1", stopped)
	}
}

// A paused task has no running goroutine, so finalizing IS the exclusive claim
// against a concurrent approval. Claiming after telling the host would let an
// approve slip in and resume a task already reported as cancelled.
func TestStop_PausedTaskIsClaimedBeforeTheHostIsTold(t *testing.T) {
	ctx := context.Background()
	var order []string
	var mu sync.Mutex
	h := newHarness(t)
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusInputRequired})

	h.m.cfg.Stopper = Stopper(func(context.Context, string, bool) (StopOutcome, error) {
		// By the time the host is told, the row must already be cancelled —
		// otherwise a racing approve could still reclaim it.
		task, err := h.store.Get(ctx, info.TaskID)
		if err != nil {
			return StopUnknownRun, err
		}
		mu.Lock()
		order = append(order, string(task.Status))
		mu.Unlock()
		return StopCancelled, nil
	})

	if _, err := h.m.Stop(ctx, info.TaskID, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != string(StatusCancelled) {
		t.Errorf("host saw status %v when told to stop, want cancelled — the claim came too late", order)
	}
	// And a concurrent approve loses.
	if ok, _ := h.store.ReclaimWorking(ctx, info.TaskID, h.get(t, info.TaskID).RunID); ok {
		t.Error("an approval reclaimed a task that was already cancelled")
	}
}

// A working task is the other way round: cancel the run first, or its own
// completion wins the CAS and records a success for something the user stopped.
//
// And then again once the ending is ours — a run still being launched when the
// first call went out was invisible to the host then and reachable now.
func TestStop_WorkingTaskCancelsTheRunFirstThenAgain(t *testing.T) {
	ctx := context.Background()
	var saw []string
	h := newHarness(t)
	info := h.spawn(t)
	h.m.cfg.Stopper = Stopper(func(context.Context, string, bool) (StopOutcome, error) {
		task, err := h.store.Get(ctx, info.TaskID)
		if err != nil {
			return StopUnknownRun, err
		}
		saw = append(saw, string(task.Status))
		return StopCancelled, nil
	})

	if _, err := h.m.Stop(ctx, info.TaskID, false); err != nil {
		t.Fatal(err)
	}
	if len(saw) != 2 {
		t.Fatalf("host was told %d times %v, want twice: before the claim and after it", len(saw), saw)
	}
	if saw[0] != string(StatusWorking) {
		t.Errorf("host saw %q on the first call, want working — the row was claimed too early", saw[0])
	}
	if saw[1] != string(StatusCancelled) {
		t.Errorf("host saw %q on the second call, want cancelled — it follows the claim", saw[1])
	}
	if got := h.get(t, info.TaskID).Status; got != StatusCancelled {
		t.Errorf("status = %q, want cancelled", got)
	}
}

// This Manager is not the only writer a task row has: another process
// finalizes its own tasks, a startup sweep fails orphans. A waiter that only
// listened for its OWN transitions would sit out the full timeout with the
// answer already in the store.
func TestStatus_WaitNoticesAFinalizeItDidNotMake(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	runID := h.get(t, info.TaskID).RunID

	go func() {
		time.Sleep(30 * time.Millisecond)
		// Straight to the store, as another process would.
		if _, err := h.store.Finalize(ctx, info.TaskID, runID, StatusCompleted, "done", "full"); err != nil {
			t.Error(err)
		}
	}()

	start := time.Now()
	got, err := h.m.Status(ctx, info.TaskID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %v for a finalize this Manager did not make", elapsed)
	}
}

// MaxConcurrentPerParent is a public guarantee, and the calls that test it are
// the ordinary case: several spawn_task calls in one model response run
// concurrently, so a read-then-create check would let them all through.
func TestSpawnCapHoldsUnderConcurrentSpawns(t *testing.T) {
	const limit = 2
	h := newHarness(t, func(c *Config) { c.MaxConcurrentPerParent = limit })
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "p", Input: "go"})
		}()
	}
	wg.Wait()

	live, err := h.store.ListNonTerminal(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) > limit {
		t.Fatalf("%d live tasks for a parent capped at %d", len(live), limit)
	}
}

// Config.Stopper says: "Without one, Stop still finalizes the row but cannot
// interrupt the run." Deferring the terminal state to a run that was never told
// to stop leaves the task working forever.
func TestGracefulStopFinalizesWhenNothingTookTheStop(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		tune func(*Config)
	}{
		{"no stopper", func(c *Config) { c.Stopper = nil }},
		{"stopper fails", func(c *Config) {
			c.Stopper = Stopper(func(context.Context, string, bool) (StopOutcome, error) {
				return StopUnknownRun, errors.New("run already gone")
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.tune)
			info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "p", Input: "work"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.m.Stop(ctx, info.TaskID, true); err != nil {
				t.Fatalf("stop: %v", err)
			}
			got, err := h.store.Get(ctx, info.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Status.Terminal() {
				t.Fatalf("status is %q after a successful graceful Stop — the row was never finalized", got.Status)
			}
		})
	}
}

// The counterpart: when a Stopper does take a graceful stop, the terminal
// state stays with the run, which reports it through OnRunFinished.
func TestGracefulStopLeavesTheOutcomeToTheRun(t *testing.T) {
	h := newHarness(t) // harness Stopper succeeds
	ctx := context.Background()

	info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "p", Input: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Stop(ctx, info.TaskID, true); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.Get(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Terminal() {
		t.Fatal("the row was finalized here, racing the run that was told to finish its turn")
	}
}

// A wake-up launch carries its lineage: the run whose spawn started the
// delivered task, handed to the host at launch so it can land on the run's own
// durable output (traces) instead of being re-derived from task rows or
// notification text — which a fork or a fold does not carry.
func TestNotify_WakeLaunchCarriesLineage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info, err := h.m.Spawn(ctx, SpawnRequest{
		ParentSessionID: "parent", AgentName: "worker", Input: "do it",
		ParentRunID: "run_origin",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})
	h.m.DrainPending(ctx, "parent")

	wakes := h.launcher.wakes()
	if len(wakes) == 0 {
		t.Fatal("no wake launch recorded")
	}
	if wakes[0].ParentRunID != "run_origin" {
		t.Fatalf("wake lineage = %q, want run_origin", wakes[0].ParentRunID)
	}
}
