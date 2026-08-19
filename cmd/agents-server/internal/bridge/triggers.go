package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ErrTriggerDisabled is returned by a fire of a trigger that is switched off.
var ErrTriggerDisabled = errors.New("trigger is disabled")

// cronParser reads the five-field form and the descriptors (@hourly, @every
// 10m); no seconds field — a workflow is not a per-second job.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// MinEveryInterval is the shortest @every a trigger may ask for: the field
// form cannot go below a minute, and neither may the descriptor — a workflow
// starting every few seconds is a mistake, and each tick past the session's
// cap would only log a refusal.
const MinEveryInterval = time.Minute

// ValidateCronSchedule reports whether expr is a schedule the scheduler runs.
func ValidateCronSchedule(expr string) error {
	expr = strings.TrimSpace(expr)
	if _, err := cronParser.Parse(expr); err != nil {
		return fmt.Errorf("schedule %q: %w", expr, err)
	}
	if rest, ok := strings.CutPrefix(expr, "@every "); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(rest)); err == nil && d < MinEveryInterval {
			return fmt.Errorf("schedule %q: @every may not be shorter than %s", expr, MinEveryInterval)
		}
	}
	return nil
}

// TriggerScheduler fires triggers: cron ones on their schedule from a table it
// keeps in step with the store (Sync), webhook and manual ones through Fire.
// Every fire is the same start a person's Run… makes (RunWorkflow); what it
// did is recorded on the trigger. Ticks missed while the process was down are
// not replayed.
type TriggerScheduler struct {
	runner *Runner
	store  *store.TriggerStore
	cron   *cron.Cron
	stop   chan struct{}

	mu      sync.Mutex
	entries map[string]heldEntry // trigger id → what the clock holds for it
}

// heldEntry is one trigger as the clock has it: the cron entry, and the
// schedule it was added with — what a Sync compares the row against before
// touching the entry.
type heldEntry struct {
	id       cron.EntryID
	schedule string
}

// reconcileEvery is how often the clock re-reads every trigger it holds: a
// trigger deleted by a cascade (its workflow's, its session's) leaves the
// store with no Sync, and would otherwise tick until its next fire found it
// gone — a year for @yearly.
const reconcileEvery = time.Minute

// NewTriggerScheduler returns a scheduler over the runner and store; Start
// loads the table and begins ticking.
func NewTriggerScheduler(r *Runner, s *store.TriggerStore) *TriggerScheduler {
	return &TriggerScheduler{
		runner:  r,
		store:   s,
		cron:    cron.New(cron.WithParser(cronParser)),
		stop:    make(chan struct{}),
		entries: map[string]heldEntry{},
	}
}

// Start schedules every enabled cron trigger and starts the clock. A row whose
// schedule no longer parses is skipped and logged, not fatal.
func (s *TriggerScheduler) Start(ctx context.Context) error {
	all, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	for i := range all {
		s.Sync(ctx, all[i].ID)
	}
	s.cron.Start()
	go s.reconcileLoop(ctx)
	return nil
}

// Stop halts the clock and the reconcile; a fire in progress finishes on its
// own.
func (s *TriggerScheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	if s.cron != nil {
		s.cron.Stop()
	}
}

// reconcileLoop re-syncs every held trigger against the store on a timer, so
// the clock never keeps what the table has lost.
func (s *TriggerScheduler) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile brings the clock in step with the store in BOTH directions: every
// held entry (a row deleted or disabled under it comes off), and every
// enabled cron row the clock does not hold (a create or enable whose Sync was
// lost — a request that hung up mid-way, a store error — goes on).
func (s *TriggerScheduler) reconcile(ctx context.Context) {
	ids := map[string]bool{}
	s.mu.Lock()
	for id := range s.entries {
		ids[id] = true
	}
	s.mu.Unlock()
	if rows, err := s.store.List(ctx); err == nil {
		for i := range rows {
			if rows[i].Enabled && rows[i].Kind == store.TriggerKindCron {
				ids[rows[i].ID] = true
			}
		}
	} else {
		logging.Ctx(ctx).Warn("reconciling triggers: store not read; held entries only", "error", err)
	}
	for id := range ids {
		s.Sync(ctx, id)
	}
}

// Sync brings the clock in step with one trigger AS THE STORE NOW HAS IT:
// read by id under the scheduler's lock, so two syncs racing over the same
// trigger both apply the latest row rather than each its own stale snapshot
// (an "enable" landing after a "disable" would otherwise leave a disabled
// trigger ticking). Scheduled when it is an enabled cron trigger, off the
// clock otherwise — a row that is gone included. An entry the row still
// matches (same schedule, still enabled) is LEFT ALONE: re-adding an entry
// restarts its clock, and an @every interval is measured from when it was
// added — a re-add every reconcile would push "@every 30m" out forever.
// Called after every create, update and delete, by the reconcile, and by a
// fire that finds its trigger gone.
func (s *TriggerScheduler) Sync(ctx context.Context, triggerID string) {
	// Detached: the write this follows has landed, and the request that made
	// it may already be gone; a clock left out of step until the reconcile
	// (or a restart) is not what a hung-up client should cost.
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.store.Get(ctx, triggerID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		logging.Ctx(ctx).Warn("syncing a trigger: not read; left as it was", "error", err, "trigger_id", triggerID)
		return
	}
	wanted := err == nil && t.Enabled && t.Kind == store.TriggerKindCron
	held, holding := s.entries[triggerID]
	if holding && wanted && held.schedule == t.Schedule {
		return // in step already; do not restart its clock
	}
	if holding {
		s.cron.Remove(held.id)
		delete(s.entries, triggerID)
	}
	if !wanted {
		return
	}
	entry, err := s.cron.AddFunc(t.Schedule, func() {
		// The hub's root context: a fire outlives nothing but the process.
		root := s.runner.hub.rootCtx
		if _, err := s.Fire(root, triggerID, ""); err != nil {
			logging.Ctx(root).Warn("cron trigger did not fire", "error", err, "trigger_id", triggerID)
		}
	})
	if err != nil {
		logging.Ctx(ctx).Warn("trigger schedule not scheduled", "error", err, "trigger_id", t.ID, "schedule", t.Schedule)
		return
	}
	s.entries[t.ID] = heldEntry{id: entry, schedule: t.Schedule}
}

// ErrTriggerTarget marks a fire refused because what the trigger names is
// gone — an agent deleted since. The trigger stays, to be re-pointed.
var ErrTriggerTarget = errors.New("trigger target unavailable")

// Fired is what a fire started: a workflow execution (the task) or an agent
// turn (the run).
type Fired struct {
	*TaskInfo
	RunID string `json:"run_id,omitempty"`
}

// Fire starts what the trigger names now — its workflow, or a turn of its
// agent — with its brief led by payload when there is one (a webhook's
// body), and records the outcome on the trigger. A disabled trigger does not
// fire; a session at its background cap, or busy with a run, refuses like
// any start would, and that refusal is what the trigger then shows.
func (s *TriggerScheduler) Fire(ctx context.Context, triggerID, payload string) (*Fired, error) {
	t, err := s.store.Get(ctx, triggerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deleted under the clock — with its workflow or its session, which
			// cascade without a Sync — so this tick is its last.
			s.Sync(ctx, triggerID)
		}
		return nil, err
	}
	if !t.Enabled {
		return nil, ErrTriggerDisabled
	}
	input := t.Brief
	if p := strings.TrimSpace(payload); p != "" {
		input += "\n\nPayload:\n" + p
	}
	var fired *Fired
	var ferr error
	if t.Target == store.TriggerTargetAgent {
		fired, ferr = s.fireAgentTurn(ctx, t, input)
	} else {
		fired, ferr = s.fireWorkflow(ctx, t, input)
	}
	startedID, msg := "", ""
	switch {
	case ferr != nil:
		msg = ferr.Error()
	case fired.TaskInfo != nil:
		startedID = fired.TaskID
	default:
		startedID = fired.RunID
	}
	if rerr := s.store.RecordFire(context.WithoutCancel(ctx), t.ID, startedID, msg); rerr != nil {
		logging.Ctx(ctx).Warn("recording a trigger fire", "error", rerr, "trigger_id", t.ID)
	}
	return fired, ferr
}

// fireWorkflow is the workflow target: the same start a person's Run… makes.
func (s *TriggerScheduler) fireWorkflow(ctx context.Context, t *store.Trigger, input string) (*Fired, error) {
	// A trigger whose workflow is gone (a delete that raced its creation —
	// the cascade never saw it) can never fire again: it goes the way the
	// cascade would have taken it. A missing SESSION or AGENT stays: the row
	// is visible and the person can re-point it.
	if _, werr := s.runner.Deps.Workflows.Get(ctx, t.WorkflowID); errors.Is(werr, store.ErrNotFound) {
		// Only while it still names that workflow: re-pointed under this fire,
		// it is a live trigger again and stays.
		if derr := s.store.DeleteIfWorkflow(ctx, t.ID, t.WorkflowID); derr != nil && !errors.Is(derr, store.ErrNotFound) {
			logging.Ctx(ctx).Warn("removing a trigger whose workflow is gone", "error", derr, "trigger_id", t.ID)
		}
		s.Sync(ctx, t.ID)
		return nil, fmt.Errorf("trigger %s: %w", t.ID, werr)
	}
	info, err := s.runner.RunWorkflow(ctx, t.WorkflowID, t.SessionID, input, store.OriginOf(t))
	if err != nil {
		return nil, err
	}
	return &Fired{TaskInfo: info}, nil
}

// fireAgentTurn is the agent target: the brief sent as a message of the
// session, run by the trigger's agent — the same run the composer makes,
// under the session's own sandbox binding. A note goes on the conversation
// first, so the reader sees the next question was an automation's.
func (s *TriggerScheduler) fireAgentTurn(ctx context.Context, t *store.Trigger, input string) (*Fired, error) {
	sess, err := s.runner.Deps.Sessions.Get(ctx, t.SessionID)
	if err != nil {
		return nil, err
	}
	if sess.Hidden {
		return nil, fmt.Errorf("session %s is a task's own; a trigger prompts a conversation", t.SessionID)
	}
	agent, err := s.runner.Deps.AgentConfigs.Get(ctx, t.AgentConfigID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The trigger stands, to be re-pointed; the fire is refused, not
			// faulted, and not "trigger not found".
			return nil, fmt.Errorf("%w: agent %s of trigger %s is gone", ErrTriggerTarget, t.AgentConfigID, t.ID)
		}
		return nil, fmt.Errorf("agent: %w", err)
	}
	runID := store.NewID()
	// The note is written once the run holds the session — after the
	// reservation, before the launch — so it precedes the message the run
	// persists, and a turn refused (busy, cap, deleting) leaves no note for
	// a turn that never happened. Detached: a hand fire's request may be
	// over by then, and the run outlives it anyway.
	noteCtx := context.WithoutCancel(ctx)
	note := func() {
		ref, rerr := store.RefFor(noteCtx, s.runner.db, t.SessionID)
		if rerr != nil {
			return
		}
		tf := store.TriggerFired{RunID: runID, AgentConfigID: agent.ID, AgentName: agent.Name, Brief: input, Origin: store.OriginOf(t)}
		if aerr := store.NewEntryStoreFor(s.runner.db, ref).AppendTriggerFired(noteCtx, ref, tf); aerr != nil {
			logging.Ctx(noteCtx).Warn("recording the trigger-fired note", "error", aerr, "trigger_id", t.ID)
		}
	}
	if _, err := s.runner.startRunReserved(runID, t.SessionID, agent.ID, "", "", input, "", nil, nil, note); err != nil {
		return nil, err
	}
	return &Fired{RunID: runID}, nil
}
