package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// triggerFixture is a workflow fixture with a trigger store and a scheduler
// over it (not started — each test decides).
func triggerFixture(t *testing.T, modelURL string) (*Runner, *store.Session, *store.Workflow, *TriggerScheduler) {
	t.Helper()
	runner, sess, wf := workflowFixture(t, modelURL)
	ts := store.NewTriggerStore(runner.db)
	return runner, sess, wf, NewTriggerScheduler(runner, ts)
}

func TestValidateCronSchedule(t *testing.T) {
	for _, ok := range []string{"0 9 * * 1-5", "*/15 * * * *", "@hourly", "@every 10m", " @daily "} {
		if err := ValidateCronSchedule(ok); err != nil {
			t.Errorf("%q: %v, want accepted", ok, err)
		}
	}
	// Prose, a seconds field, and an @every under a minute are not schedules
	// here.
	for _, bad := range []string{"every day at nine", "* * * * * *", "", "@every", "@every 30s", "@every 1s"} {
		if err := ValidateCronSchedule(bad); err == nil {
			t.Errorf("%q: accepted, want refused", bad)
		}
	}
}

// A fire is the same start a person's Run… makes: an execution of the
// workflow on the trigger's session, led by its brief and the payload — and
// what happened is written on the trigger. A disabled trigger does not fire.
func TestTriggerFireStartsTheWorkflowAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	trg := &store.Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: store.TriggerKindWebhook, Brief: "nightly review", Enabled: true}
	if err := sched.store.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}

	info, err := sched.Fire(ctx, trg.ID, `{"pr": 42}`)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 15*time.Second)
	if done.Status != "completed" || done.ParentSessionID != sess.ID {
		t.Fatalf("task = %s on %s (%s), want completed on the trigger's session", done.Status, done.ParentSessionID, done.Summary)
	}
	if !strings.HasPrefix(st.Input, "nightly review") || !strings.Contains(st.Input, "Payload:\n{\"pr\": 42}") {
		t.Fatalf("brief = %q, want the trigger's brief led, the payload appended", st.Input)
	}
	rec, err := sched.store.Get(ctx, trg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LastStartedID != info.TaskID || rec.LastError != "" || rec.LastFiredAt.IsZero() {
		t.Fatalf("recorded fire = %+v, want the task it started", rec)
	}
	// The start left its note on the conversation, saying which trigger.
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, v := range views {
		if v.Display != nil && v.Display.Kind == store.DisplayWorkflowStarted {
			origin, _ := v.Display.Extra["origin"].(map[string]any)
			if origin["kind"] != store.OriginTrigger || origin["trigger_id"] != trg.ID || origin["trigger_kind"] != store.TriggerKindWebhook {
				t.Fatalf("note origin = %v, want this webhook trigger", origin)
			}
			noted = true
		}
	}
	if !noted {
		t.Fatal("a trigger's start must leave its note on the conversation")
	}

	// Switched off, it starts nothing.
	trg.Enabled = false
	if err := sched.store.Update(ctx, trg.ID, trg); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Fire(ctx, trg.ID, ""); !errors.Is(err, ErrTriggerDisabled) {
		t.Fatalf("fire of a disabled trigger = %v, want ErrTriggerDisabled", err)
	}
	// A fire that cannot start (the session is gone) is recorded as the
	// trigger's last error, not lost in a log.
	trg.Enabled, trg.SessionID = true, "nope"
	if err := sched.store.Update(ctx, trg.ID, trg); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Fire(ctx, trg.ID, ""); err == nil {
		t.Fatal("a fire into a missing session must fail")
	}
	rec, _ = sched.store.Get(ctx, trg.ID)
	if rec.LastError == "" || rec.LastStartedID != "" {
		t.Fatalf("recorded fire = %+v, want the failure and no task", rec)
	}
	// A trigger whose WORKFLOW is gone is removed at its next fire — it could
	// never fire again and is listed nowhere.
	trg.SessionID = sess.ID
	if err := sched.store.Update(ctx, trg.ID, trg); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.db.NewDelete().Model((*store.Workflow)(nil)).Where("id = ?", wf.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Fire(ctx, trg.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fire without a workflow = %v, want not found", err)
	}
	if _, err := sched.store.Get(ctx, trg.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the orphaned trigger must be removed: %v", err)
	}
}

// Only an enabled cron trigger is on the clock; Sync follows every change,
// and a tick starts the workflow like any fire.
func TestTriggerSchedulerFollowsTheTable(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	hook := &store.Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: store.TriggerKindWebhook, Brief: "b", Enabled: true}
	tick := &store.Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: store.TriggerKindCron, Schedule: "@every 1s", Brief: "on the clock", Enabled: false}
	for _, trg := range []*store.Trigger{hook, tick} {
		if err := sched.store.Create(ctx, trg); err != nil {
			t.Fatal(err)
		}
	}
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()
	if n := len(sched.entries); n != 0 {
		t.Fatalf("%d entries scheduled, want none: a webhook is never on the clock and the cron one is off", n)
	}

	tick.Enabled = true
	if err := sched.store.Update(ctx, tick.ID, tick); err != nil {
		t.Fatal(err)
	}
	sched.Sync(ctx, tick.ID)
	held, ok := sched.entries[tick.ID]
	if !ok {
		t.Fatal("an enabled cron trigger must be on the clock after Sync")
	}
	// A reconcile (and any Sync) over an unchanged row leaves the entry
	// alone — the SAME cron entry, its clock untouched: re-adding restarts an
	// @every interval from now, and a reconcile every minute would push
	// "@every 30m" out forever.
	next := sched.cron.Entry(held.id).Next
	sched.reconcile(ctx)
	sched.Sync(ctx, tick.ID)
	if again := sched.entries[tick.ID]; again.id != held.id || sched.cron.Entry(again.id).Next != next {
		t.Fatalf("an unchanged trigger was re-added: entry %v→%v next %v→%v", held.id, again.id, next, sched.cron.Entry(again.id).Next)
	}
	// It ticks: within a few seconds the session has an execution of the
	// workflow, briefed by the trigger.
	deadline := time.Now().Add(6 * time.Second)
	for {
		rows, err := runner.Deps.Tasks.ListByParent(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			st, err := store.DecodeWorkflowState(rows[0].State)
			if err != nil || st.Input != "on the clock" {
				t.Fatalf("ticked task state = %+v (%v), want the trigger's brief", st, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cron trigger never fired")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Off again — and off the clock.
	tick.Enabled = false
	if err := sched.store.Update(ctx, tick.ID, tick); err != nil {
		t.Fatal(err)
	}
	sched.Sync(ctx, tick.ID)
	if _, ok := sched.entries[tick.ID]; ok {
		t.Fatal("a disabled trigger must leave the clock")
	}
	// Sync reads the store, not its caller: a stale "enabled" snapshot cannot
	// put a disabled trigger back on the clock.
	sched.Sync(ctx, tick.ID)
	if _, ok := sched.entries[tick.ID]; ok {
		t.Fatal("a sync must apply the row as the store has it")
	}
	// A trigger deleted UNDER the clock — the workflow's or the session's
	// cascade, which no Sync sees — is dropped by the periodic reconcile
	// (long before its next tick, for a slow schedule)…
	tick.Enabled = true
	if err := sched.store.Update(ctx, tick.ID, tick); err != nil {
		t.Fatal(err)
	}
	sched.Sync(ctx, tick.ID)
	if _, err := runner.db.NewDelete().Model((*store.Trigger)(nil)).Where("id = ?", tick.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	sched.reconcile(ctx)
	if _, ok := sched.entries[tick.ID]; ok {
		t.Fatal("reconcile must drop a trigger the store no longer has")
	}
	if err := sched.store.Create(ctx, tick); err != nil {
		t.Fatal(err)
	}
	// …and, should a tick come first, leaves at that tick.
	tick.Enabled = true
	if err := sched.store.Update(ctx, tick.ID, tick); err != nil {
		t.Fatal(err)
	}
	sched.Sync(ctx, tick.ID)
	if err := runner.Deps.Workflows.Delete(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Fire(ctx, tick.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fire of a cascaded-away trigger = %v, want not found", err)
	}
	if _, ok := sched.entries[tick.ID]; ok {
		t.Fatal("a trigger deleted with its workflow must leave the clock at its next tick")
	}
}

// An agent-target trigger sends its brief as a message of the session, run
// by the trigger's agent — an ordinary turn, not a background task — with a
// note before it saying an automation asked. A session busy with a run
// refuses, and that refusal is what the trigger then shows.
func TestTriggerFireRunsAnAgentTurn(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	agentID := wf.Steps[0].AgentConfigID
	trg := &store.Trigger{Target: store.TriggerTargetAgent, AgentConfigID: agentID, SessionID: sess.ID, Kind: store.TriggerKindCron, Schedule: "@daily", Brief: "morning: what changed?", Enabled: true}
	if err := sched.store.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}

	fired, err := sched.Fire(ctx, trg.ID, "")
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if fired.TaskInfo != nil || fired.RunID == "" {
		t.Fatalf("fired = %+v, want a run, not a task", fired)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, ok := runner.hub.Info(fired.RunID); ok && info.Status != RunRunning {
			if info.Status != RunCompleted {
				t.Fatalf("run = %s, want completed", info.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent turn did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec, err := sched.store.Get(ctx, trg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.LastStartedID != fired.RunID || rec.LastError != "" {
		t.Fatalf("record = started %q error %q, want the run id and no error", rec.LastStartedID, rec.LastError)
	}
	// The conversation: the note, then the turn — the message led by the
	// brief, in the session itself.
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.NewEntryStoreFor(runner.db, ref).Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	noteAt, msgAt := -1, -1
	for i, e := range entries {
		if e.Display != nil && e.Display.Kind == store.DisplayTriggerFired {
			noteAt = i
		}
		if e.Kind == session.EntryKindItem && msgAt < 0 && strings.Contains(string(e.Item), "morning: what changed?") {
			msgAt = i
		}
	}
	if noteAt < 0 || msgAt < 0 || noteAt > msgAt {
		t.Fatalf("entries: note at %d, message at %d — want the note before the message", noteAt, msgAt)
	}

	// Busy: a run in flight on the session refuses the turn, and the trigger
	// records that.
	seg, _, err := runner.hub.register("held-run", sess.ID, agentID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { seg.finalize(); runner.hub.finish("held-run", false) }()
	if _, err := sched.Fire(ctx, trg.ID, ""); !errors.As(err, new(ErrSessionBusy)) {
		t.Fatalf("Fire on a busy session = %v, want ErrSessionBusy", err)
	}
	rec, err = sched.store.Get(ctx, trg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.LastError, "active run") {
		t.Fatalf("last_error = %q, want the busy refusal", rec.LastError)
	}
}
