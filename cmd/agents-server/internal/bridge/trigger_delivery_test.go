package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// holdSession occupies the session with a run that is not the trigger's, and
// returns what ends it.
func holdSession(t *testing.T, runner *Runner, sessionID, agentID string) func() {
	t.Helper()
	runID := store.NewID()
	seg, _, err := runner.hub.register(runID, sessionID, "", agentID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return func() { seg.finalize(); runner.hub.finish(runID, false) }
}

// agentTrigger creates an enabled trigger of the given kind that sends its
// brief to the session as a turn of the fixture's first agent.
func agentTrigger(t *testing.T, sched *TriggerScheduler, sess *store.Session, wf *store.Workflow, kind string) *store.Trigger {
	t.Helper()
	trg := &store.Trigger{Target: store.TriggerTargetAgent, AgentConfigID: wf.Steps[0].AgentConfigID, SessionID: sess.ID, Kind: kind, Brief: "triage this", Enabled: true}
	if kind == store.TriggerKindCron {
		trg.Schedule = "@daily"
	}
	if err := sched.store.Create(context.Background(), trg); err != nil {
		t.Fatal(err)
	}
	return trg
}

// owedBy returns the deliveries the trigger still has waiting.
func owedBy(t *testing.T, runner *Runner, trg *store.Trigger) []store.Wakeup {
	t.Helper()
	owed, err := runner.Deps.Wakeups.PendingFor(context.Background(), store.WakeKindTrigger, trg.ID)
	if err != nil {
		t.Fatal(err)
	}
	return owed
}

// firedBriefs returns what each turn a trigger started on the session was
// told, in order: one per trigger-fired note.
func firedBriefs(t *testing.T, runner *Runner, sessionID string) []string {
	t.Helper()
	ctx := context.Background()
	ref, err := store.RefFor(ctx, runner.db, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.NewEntryStoreFor(runner.db, ref).Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var briefs []string
	for _, e := range entries {
		if e.Display != nil && e.Display.Kind == store.DisplayTriggerFired {
			brief, _ := e.Display.Extra["brief"].(string)
			briefs = append(briefs, brief)
		}
	}
	return briefs
}

// awaitTurns waits until the trigger has started n turns on the session and
// the last of them has ended.
func awaitTurns(t *testing.T, runner *Runner, sessionID string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		// The note is written by a run that already holds the session, so n
		// notes and no live run means the nth turn is over.
		briefs := firedBriefs(t, runner, sessionID)
		if _, live := runner.hub.ActiveRunForSession(sessionID); len(briefs) >= n && !live {
			return briefs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d turns started, want %d finished", len(briefs), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWaitsNamesTheRefusalsThatPass(t *testing.T) {
	for _, err := range []error{
		ErrSessionBusy{RunID: "r"}, ErrSessionAwaitingApproval, ErrShuttingDown{},
		fmt.Errorf("spawning: %w", tasks.ErrTaskLimit),
	} {
		if !waits(err) {
			t.Errorf("waits(%v) = false, want a delivery to wait this out", err)
		}
	}
	for _, err := range []error{nil, ErrTriggerTarget, ErrWorkflowUnavailable, ErrSessionDeleting{SessionID: "s"}, store.ErrNotFound} {
		if waits(err) {
			t.Errorf("waits(%v) = true, want it to end the delivery", err)
		}
	}
}

// A delivery to a busy session is owed, not refused: it waits without a mark
// against the trigger, and when the session frees ONE turn carries the whole
// backlog — the brief once, each payload in its own block.
func TestDeliveryWaitsForABusySessionAndIsBatched(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	trg := agentTrigger(t, sched, sess, wf, store.TriggerKindWebhook)
	release := holdSession(t, runner, sess.ID, trg.AgentConfigID)

	for i, key := range []string{"sig-1", "sig-2"} {
		fired, err := sched.Deliver(ctx, trg.ID, fmt.Sprintf(`{"n":%d}`, i+1), key)
		if fired != nil || err != nil {
			t.Fatalf("Deliver on a busy session = %+v, %v; want it to wait", fired, err)
		}
	}
	// Waiting is still a delivery accepted: the same one again is a replay.
	if _, err := sched.Deliver(ctx, trg.ID, `{"n":1}`, "sig-1"); !errors.Is(err, ErrDeliveryReplayed) {
		t.Fatalf("a waiting delivery sent again = %v, want ErrDeliveryReplayed", err)
	}
	if owed := owedBy(t, runner, trg); len(owed) != 2 {
		t.Fatalf("%d deliveries waiting, want 2", len(owed))
	}
	rec, err := sched.store.Get(ctx, trg.ID)
	if err != nil || rec.LastError != "" || rec.LastStartedID != "" {
		t.Fatalf("record while waiting = %+v (%v), want nothing: waiting is not a failure", rec, err)
	}
	// A drain while the session is still held starts nothing and loses nothing.
	runner.drainWakeups(ctx, sess.ID)
	if owed := owedBy(t, runner, trg); len(owed) != 2 {
		t.Fatalf("%d deliveries waiting after a drain on a busy session, want 2", len(owed))
	}

	release()
	runner.drainWakeups(ctx, sess.ID)
	briefs := awaitTurns(t, runner, sess.ID, 1)
	if len(briefs) != 1 {
		t.Fatalf("%d turns, want one for the whole backlog", len(briefs))
	}
	got := briefs[0]
	if strings.Count(got, "triage this") != 1 || strings.Count(got, "<external source=\"webhook\"") != 2 ||
		strings.Index(got, `{"n":1}`) > strings.Index(got, `{"n":2}`) {
		t.Fatalf("turn = %q, want the brief once and both payloads framed, oldest first", got)
	}
	if owed := owedBy(t, runner, trg); len(owed) != 0 {
		t.Fatalf("%d deliveries still waiting after the turn", len(owed))
	}
	rec, err = sched.store.Get(ctx, trg.ID)
	if err != nil || rec.LastStartedID == "" || rec.LastError != "" {
		t.Fatalf("record after the turn = %+v (%v), want the run it started", rec, err)
	}
}

// One start carries no more than one delivery may: a backlog past that takes
// a turn each, the next started by the end of the one before.
func TestBacklogPastOneDeliverysSizeTakesAnotherTurn(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	trg := agentTrigger(t, sched, sess, wf, store.TriggerKindWebhook)
	release := holdSession(t, runner, sess.ID, trg.AgentConfigID)
	big := strings.Repeat("x", FirePayloadLimit*2/3)
	for _, key := range []string{"a", "b"} {
		if fired, err := sched.Deliver(ctx, trg.ID, key+big, key); fired != nil || err != nil {
			t.Fatalf("Deliver = %+v, %v; want it to wait", fired, err)
		}
	}
	release()
	runner.drainWakeups(ctx, sess.ID)
	briefs := awaitTurns(t, runner, sess.ID, 2)
	if len(briefs) != 2 || !strings.Contains(briefs[0], "a"+big) || strings.Contains(briefs[0], "b"+big) || !strings.Contains(briefs[1], "b"+big) {
		t.Fatalf("%d turns, want the two payloads in a turn each, oldest first", len(briefs))
	}
}

// A paused approval is waited out too (invariant 19), and the restart drain
// pays what was owed when the process went down.
func TestDeliveryWaitsOutAPausedApproval(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	pausedID := store.NewID()
	pausedRun(t, runner, sess.ID, pausedID)
	trg := agentTrigger(t, sched, sess, wf, store.TriggerKindCron)

	if fired, err := sched.Deliver(ctx, trg.ID, "", "tick-1"); fired != nil || err != nil {
		t.Fatalf("Deliver on a paused session = %+v, %v; want it to wait", fired, err)
	}
	// A tick already owed stands for the next one.
	if fired, err := sched.Deliver(ctx, trg.ID, "", "tick-2"); fired != nil || err != nil {
		t.Fatalf("a second tick = %+v, %v; want it folded into the one waiting", fired, err)
	}
	if owed := owedBy(t, runner, trg); len(owed) != 1 || owed[0].Attempt != "tick-1" {
		t.Fatalf("waiting = %+v, want the first tick alone", owed)
	}
	if rows, err := runner.Deps.PendingApprovals.ListBySession(ctx, sess.ID); err != nil || len(rows) != 1 {
		t.Fatalf("pending approvals = %d (%v), want the pause left standing", len(rows), err)
	}
	if briefs := firedBriefs(t, runner, sess.ID); len(briefs) != 0 {
		t.Fatalf("%d turns started under a pause, want none", len(briefs))
	}

	if err := runner.Deps.PendingApprovals.Delete(ctx, pausedID); err != nil {
		t.Fatal(err)
	}
	runner.DrainPendingWakeups(ctx)
	if briefs := awaitTurns(t, runner, sess.ID, 1); len(briefs) != 1 || briefs[0] != "triage this" {
		t.Fatalf("turns = %q, want the one tick's, the brief alone", briefs)
	}
}

// An approval nobody answers does not hold a delivery for good: when it
// expires, the reaper starts what waited behind it.
func TestExpiredApprovalStartsWhatWaited(t *testing.T) {
	ctx := t.Context()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	if err := store.NewSettingStore(runner.db).Set(ctx, settings.KeyApprovalTTLMinutes, "1"); err != nil {
		t.Fatal(err)
	}
	pausedID := store.NewID()
	pausedRun(t, runner, sess.ID, pausedID)
	trg := agentTrigger(t, sched, sess, wf, store.TriggerKindCron)
	if fired, err := sched.Deliver(ctx, trg.ID, "", "tick-1"); fired != nil || err != nil {
		t.Fatalf("Deliver on a paused session = %+v, %v; want it to wait", fired, err)
	}
	if _, err := runner.db.NewUpdate().Model((*store.PendingApproval)(nil)).
		Set("created_at = ?", time.Now().Add(-2*time.Hour)).Where("run_id = ?", pausedID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	go runner.RunApprovalReaper(ctx)
	if briefs := awaitTurns(t, runner, sess.ID, 1); len(briefs) != 1 {
		t.Fatalf("%d turns after the approval expired, want the one that waited", len(briefs))
	}
}

// A trigger may have only so many deliveries waiting.
func TestDeliveryBacklogIsCapped(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	trg := agentTrigger(t, sched, sess, wf, store.TriggerKindWebhook)
	defer holdSession(t, runner, sess.ID, trg.AgentConfigID)()
	for i := range maxTriggerBacklog {
		if _, err := sched.Deliver(ctx, trg.ID, "p", fmt.Sprint("sig-", i)); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if _, err := sched.Deliver(ctx, trg.ID, "p", "one-more"); !errors.Is(err, ErrTriggerBacklog) {
		t.Fatalf("a delivery past the cap = %v, want ErrTriggerBacklog", err)
	}
}

// What waits for a trigger that is then switched off or deleted is cancelled
// at the next drain, and starts nothing.
func TestWaitingDeliveriesOfATriggerThatNoLongerFiresAreCancelled(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	off := agentTrigger(t, sched, sess, wf, store.TriggerKindWebhook)
	gone := agentTrigger(t, sched, sess, wf, store.TriggerKindWebhook)
	release := holdSession(t, runner, sess.ID, off.AgentConfigID)
	for _, trg := range []*store.Trigger{off, gone} {
		if fired, err := sched.Deliver(ctx, trg.ID, "p", "sig"); fired != nil || err != nil {
			t.Fatalf("Deliver = %+v, %v; want it to wait", fired, err)
		}
	}
	off.Enabled = false
	if err := sched.store.UpdateSettings(ctx, off.ID, off); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.db.NewDelete().Model((*store.Trigger)(nil)).Where("id = ?", gone.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	runner.drainWakeups(ctx, sess.ID)

	var rows []store.Wakeup
	if err := runner.db.NewSelect().Model(&rows).Where("session_id = ?", sess.ID).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].State != store.WakeCancelled || rows[1].State != store.WakeCancelled {
		t.Fatalf("debts = %+v, want both cancelled", rows)
	}
	if briefs := firedBriefs(t, runner, sess.ID); len(briefs) != 0 {
		t.Fatalf("%d turns started, want none", len(briefs))
	}
}

// A delivery that cannot start for a reason that will not pass is not kept:
// the sender is told, and the same delivery may be sent again once it is fixed.
func TestDeliveryThatCannotStartIsForgotten(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	trg := agentTrigger(t, sched, sess, wf, store.TriggerKindWebhook)
	// Written around the store, whose cascade would take the trigger with it.
	if _, err := runner.db.NewUpdate().Model((*store.Trigger)(nil)).
		Set("agent_config_id = ?", store.NewID()).Where("id = ?", trg.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := sched.Deliver(ctx, trg.ID, "p", "sig"); !errors.Is(err, ErrTriggerTarget) {
			t.Fatalf("Deliver to a trigger whose agent is gone = %v, want ErrTriggerTarget both times, never a replay", err)
		}
	}
	rec, err := sched.store.Get(ctx, trg.ID)
	if err != nil || rec.LastError == "" {
		t.Fatalf("record = %+v (%v), want the failure shown on the trigger", rec, err)
	}
	var rows []store.Wakeup
	if err := runner.db.NewSelect().Model(&rows).Where("source_id = ?", trg.ID).Scan(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("debts = %+v (%v), want none kept", rows, err)
	}
}

// A workflow starts beside a run, so only the task cap makes its delivery
// wait — and a task ending is the boundary that starts it.
func TestWorkflowDeliveryWaitsAtTheTaskCap(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	trg := &store.Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: store.TriggerKindWebhook, Brief: "nightly review", Enabled: true}
	if err := sched.store.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}
	defer holdSession(t, runner, sess.ID, wf.Steps[0].AgentConfigID)()
	var fillers []string
	for range runner.Deps.SettingReader.Int(ctx, settings.KeyMaxTasksPerSession) {
		id := store.NewID()
		fillers = append(fillers, id)
		if err := runner.Deps.Tasks.Create(ctx, &store.Task{
			ID: id, RunID: store.NewID(), ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Label: "other", Status: "working",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if fired, err := sched.Deliver(ctx, trg.ID, `{"pr":7}`, "sig"); fired != nil || err != nil {
		t.Fatalf("Deliver at the task cap = %+v, %v; want it to wait", fired, err)
	}
	if owed := owedBy(t, runner, trg); len(owed) != 1 {
		t.Fatalf("%d deliveries waiting, want 1", len(owed))
	}

	if _, err := runner.db.NewDelete().Model((*store.Task)(nil)).Where("id IN (?)", bun.List(fillers)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// The session is still busy: a workflow's start does not need it.
	runner.drainWakeups(ctx, sess.ID)
	if owed := owedBy(t, runner, trg); len(owed) != 0 {
		t.Fatalf("%d deliveries waiting under the cap, want it started", len(owed))
	}
	rows, err := runner.Deps.Tasks.ListByParent(ctx, sess.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("tasks = %d (%v), want the one execution", len(rows), err)
	}
	st, err := store.DecodeWorkflowState(rows[0].State)
	if err != nil || !strings.HasPrefix(st.Input, "nightly review\n\n<external source=\"webhook\"") || !strings.Contains(st.Input, `{"pr":7}`) {
		t.Fatalf("execution brief = %+v (%v), want the trigger's with the payload framed", st, err)
	}
	awaitWorkflow(t, runner, rows[0].ID, 15*time.Second)
}
