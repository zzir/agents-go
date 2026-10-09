package tasks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

const (
	// DefaultMaxConcurrentPerParent bounds tasks in flight for one parent.
	DefaultMaxConcurrentPerParent = 6
	// DefaultSummaryLimit is how much of a result reaches the notification and
	// the UI card, in runes.
	DefaultSummaryLimit = 300
	// DefaultMaxStatusWait bounds task_status's server-side wait.
	DefaultMaxStatusWait = 120 * time.Second
	// DefaultMaxDepth is how many task hops are allowed; a task spawned from an
	// ordinary session is depth 1, so 1 means a task cannot spawn tasks.
	DefaultMaxDepth = 1
	// DefaultMaxAttemptsPerTask bounds the runs one task may have, the original
	// included.
	DefaultMaxAttemptsPerTask = 3
	// DefaultMaxContinuations bounds the runs a Continue hook may chain under
	// one task before the Manager ends it.
	DefaultMaxContinuations = 50
)

// ErrTaskLimit reports that a parent session already has its maximum tasks in
// flight.
type ErrTaskLimit struct{ Limit int }

func (e ErrTaskLimit) Error() string {
	return fmt.Sprintf("tasks: this conversation already has %d tasks running; wait for one to finish", e.Limit)
}

// ErrDepthLimit reports that a task tried to spawn a task too deep.
type ErrDepthLimit struct{ Limit int }

func (e ErrDepthLimit) Error() string {
	return fmt.Sprintf("tasks: cannot spawn from a task at depth %d", e.Limit)
}

// ErrAlreadyFinal reports a stop of a task that had already finished.
type ErrAlreadyFinal struct{ Status Status }

func (e ErrAlreadyFinal) Error() string { return "tasks: task already " + string(e.Status) }

// ErrNotRetryable reports a retry of a task that is not failed.
type ErrNotRetryable struct{ Status Status }

func (e ErrNotRetryable) Error() string {
	return fmt.Sprintf("tasks: cannot retry a task that is %s; only a failed task can be retried", e.Status)
}

// ErrRetryLimit reports a task that has used every attempt it is allowed.
type ErrRetryLimit struct{ Limit int }

func (e ErrRetryLimit) Error() string {
	return fmt.Sprintf("tasks: this task has already used its %d attempts; start a new task instead", e.Limit)
}

// ErrRetryConflict reports a retry that lost its claim to another writer
// between the read and the compare-and-set. Trying again is the remedy; hosts
// map it to 409.
var ErrRetryConflict = errors.New("tasks: another writer claimed this task first; try again")

// Config configures a Manager. Store, Sessions, Resolver and Launcher are
// required.
type Config struct {
	Store    Store
	Sessions session.Repo
	Resolver AgentResolver
	Launcher Launcher
	// Stopper cancels a running task; optional (see Stopper).
	Stopper Stopper
	// MaxConcurrentPerParent resolves the cap on a parent's live tasks at each
	// spawn and retry; nil or <= 0 means DefaultMaxConcurrentPerParent. Several
	// Managers over one Store can each admit up to the cap.
	MaxConcurrentPerParent func() int
	SummaryLimit           int
	MaxStatusWait          time.Duration
	MaxDepth               int
	// MaxAttemptsPerTask bounds a task's runs, the original included; zero uses
	// DefaultMaxAttemptsPerTask, 1 disables retrying.
	MaxAttemptsPerTask int
	// MaxContinuations bounds the runs Continue may chain since the spawn or the
	// last retry; zero uses DefaultMaxContinuations — see spec §2.13.
	MaxContinuations int

	// NewID mints task, run and session ids. Nil uses a built-in generator.
	NewID func() string
	// Logger receives the Manager's own records; nil is silent.
	Logger *slog.Logger
	// OnTaskUpdate, when set, is called whenever a task's public state changes.
	OnTaskUpdate func(ctx context.Context, t *Task)
	// OnFinished, when set, is called once per terminal transition this Manager
	// claimed, with the claimed snapshot — see spec §2.13.
	OnFinished func(ctx context.Context, t *Task)
	// OnResultDelivered, when set, is called when the MODEL pulled the result
	// in-turn; a host drops its recorded debt here — see spec §2.13.
	OnResultDelivered func(ctx context.Context, t *Task)

	// DescribeState, when set, says in one line where a job of kind stands
	// ("step 2/3 (verify)"); empty adds nothing.
	DescribeState func(kind string, state json.RawMessage) string

	// Continue, when set, is asked whether a completed or failed run of the
	// current attempt ends the task: Input starts the next run, no Input is the
	// ending (Err makes it failed), nil keeps the run's outcome, an error fails
	// it — see spec §2.13.
	Continue func(ctx context.Context, t *Task, out RunOutcome) (*Continuation, error)
}

// Continuation is a Continue hook's answer: with Input, the next run and the
// State it starts from; without, the ending, with its State and Err — see spec §2.13.
type Continuation struct {
	Input string
	State json.RawMessage
	Err   error
}

// Manager owns the task lifecycle.
type Manager struct {
	cfg Config
	log *slog.Logger

	// waiters wakes task_status callers on a finalize here (awaitFinish also
	// polls); continued counts Continue-chained runs per live task.
	mu        sync.Mutex
	waiters   map[string][]chan struct{}
	continued map[string]int

	// launching holds runs whose launch has not settled, and whether the host
	// has since reported one finishing. Bounded by concurrent spawns.
	launchMu  sync.Mutex
	launching map[string]bool

	// spawning serializes Spawn and Retry per parent (count then create is a
	// read-then-write); entries are refcounted and removed on last release.
	spawnMu  sync.Mutex
	spawning map[string]*parentSpawnLock
}

// parentSpawnLock is one Manager.spawning entry: the mutex and its holder
// count, managed under spawnMu.
type parentSpawnLock struct {
	mu   sync.Mutex
	refs int
}

// lockParent holds the parent's spawn lock and returns its release, called once.
func (m *Manager) lockParent(parentSessionID string) (release func()) {
	m.spawnMu.Lock()
	if m.spawning == nil {
		m.spawning = map[string]*parentSpawnLock{}
	}
	l, ok := m.spawning[parentSessionID]
	if !ok {
		l = &parentSpawnLock{}
		m.spawning[parentSessionID] = l
	}
	l.refs++
	m.spawnMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		m.spawnMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(m.spawning, parentSessionID)
		}
		m.spawnMu.Unlock()
	}
}

// spawnLockCount reports how many per-parent locks are live. Test-only.
func (m *Manager) spawnLockCount() int {
	m.spawnMu.Lock()
	defer m.spawnMu.Unlock()
	return len(m.spawning)
}

// New returns a Manager; it panics on a Config missing a required field.
func New(cfg Config) *Manager {
	switch {
	case cfg.Store == nil:
		panic("tasks: Config.Store is required")
	case cfg.Sessions == nil:
		panic("tasks: Config.Sessions is required")
	case cfg.Resolver == nil:
		panic("tasks: Config.Resolver is required")
	case cfg.Launcher == nil:
		panic("tasks: Config.Launcher is required")
	}
	if cfg.SummaryLimit <= 0 {
		cfg.SummaryLimit = DefaultSummaryLimit
	}
	if cfg.MaxStatusWait <= 0 {
		cfg.MaxStatusWait = DefaultMaxStatusWait
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.MaxAttemptsPerTask <= 0 {
		cfg.MaxAttemptsPerTask = DefaultMaxAttemptsPerTask
	}
	if cfg.MaxContinuations <= 0 {
		cfg.MaxContinuations = DefaultMaxContinuations
	}
	if cfg.NewID == nil {
		cfg.NewID = newID
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg, log: log.With(slog.String("component", "tasks")), waiters: map[string][]chan struct{}{}}
}

// maxConcurrentPerParent resolves the live-task cap, flooring a nil resolver or
// a non-positive result at DefaultMaxConcurrentPerParent.
func (m *Manager) maxConcurrentPerParent() int {
	if m.cfg.MaxConcurrentPerParent != nil {
		if n := m.cfg.MaxConcurrentPerParent(); n > 0 {
			return n
		}
	}
	return DefaultMaxConcurrentPerParent
}

// Meta describes a session's role in the task system.
type Meta struct {
	TaskID          string
	Label           string
	ParentSessionID string
	Depth           int
}

// MetaFor reports whether a session is a task's own; a store failure is
// returned, never read as "not a task" — see spec §2.13.
func (m *Manager) MetaFor(ctx context.Context, sessionID string) (*Meta, bool, error) {
	t, err := m.cfg.Store.ByChildSession(ctx, sessionID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("tasks: looking up session %q: %w", sessionID, err)
	case t == nil:
		return nil, false, nil
	}
	return &Meta{TaskID: t.ID, Label: t.Label, ParentSessionID: t.ParentSessionID, Depth: t.Depth}, true, nil
}

// Spawn starts a background task for parentSessionID and returns as soon as
// the run is launched.
func (m *Manager) Spawn(ctx context.Context, req SpawnRequest) (*Info, error) {
	if req.ParentSessionID == "" {
		return nil, errors.New("tasks: Spawn requires a parent session")
	}

	// Depth first: a spawn past the limit resolves nothing on its way to refusing.
	depth := 1
	meta, isTask, err := m.MetaFor(ctx, req.ParentSessionID)
	if err != nil {
		return nil, err
	}
	if isTask {
		depth = meta.Depth + 1
		if depth > m.cfg.MaxDepth {
			return nil, ErrDepthLimit{Limit: meta.Depth}
		}
	}

	// The count and the create are one decision under the parent's spawn lock.
	defer m.lockParent(req.ParentSessionID)()

	// The cap is checked before anything is created: no rollback for an over-cap spawn.
	live, err := m.cfg.Store.ListNonTerminal(ctx, req.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting live tasks: %w", err)
	}
	if limit := m.maxConcurrentPerParent(); len(live) >= limit {
		return nil, ErrTaskLimit{Limit: limit}
	}

	spec, err := m.cfg.Resolver(ctx, req.ParentSessionID, req.AgentName)
	if err != nil {
		return nil, fmt.Errorf("tasks: resolving agent %q: %w", req.AgentName, err)
	}

	label := req.Label
	if label == "" {
		label = truncateRunes(req.Input, 60)
	}

	// Rollback runs on a detached context — see spec §2.13.
	cleanupCtx := context.WithoutCancel(ctx)

	// Minted, not read back: a failed read cannot leave a session nothing refers to.
	childID := m.cfg.NewID()
	if _, err := m.cfg.Sessions.Create(ctx, session.CreateOptions{
		ID:       childID,
		Title:    cmp.Or(req.Kind, "task") + ": " + label,
		Hidden:   true,
		ParentID: req.ParentSessionID,
	}); err != nil {
		return nil, fmt.Errorf("tasks: creating task session: %w", err)
	}

	task := &Task{
		ID:              m.cfg.NewID(),
		RunID:           m.cfg.NewID(),
		Label:           label,
		Kind:            req.Kind,
		ParentSessionID: req.ParentSessionID,
		ParentRunID:     req.ParentRunID,
		ToolCallID:      req.ToolCallID,
		ChildSessionID:  childID,
		Depth:           depth,
		Attempt:         1,
		Inherit:         spec.Inherit,
		State:           req.State,
		Status:          StatusWorking,
	}
	if err := m.cfg.Store.Create(ctx, task); err != nil {
		m.cleanupSession(cleanupCtx, childID)
		return nil, fmt.Errorf("tasks: creating task: %w", err)
	}

	defer m.beginLaunch(task.RunID)()
	if err := m.cfg.Launcher(ctx, launchFor(task, req.Input)); err != nil {
		// The run never started: unwind the row and the session.
		if delErr := m.cfg.Store.Delete(cleanupCtx, task.ID); delErr != nil {
			m.log.WarnContext(ctx, "unstarted task row cleanup", slog.String("task_id", task.ID),
				slog.String("error", delErr.Error()))
		}
		m.cleanupSession(cleanupCtx, childID)
		return nil, fmt.Errorf("tasks: starting task run: %w", err)
	}

	// Reconcile against a teardown that raced the launch; see settleLaunch.
	settled, err := m.settleLaunch(ctx, task.ID, task.RunID)
	if err != nil {
		return nil, err
	}
	m.notifyUpdate(ctx, settled)
	return infoFrom(settled, spec.DisplayName), nil
}

// SpawnRequest describes a task to start.
type SpawnRequest struct {
	ParentSessionID string
	AgentName       string
	Input           string
	Label           string
	// ParentRunID and ToolCallID identify the spawning turn (see Task).
	ParentRunID string
	ToolCallID  string
	// Kind and State are the host's own, copied onto the task (see Task).
	Kind  string
	State json.RawMessage
}

// launchFor is the request that starts a run of t with the given input.
func launchFor(t *Task, input string) LaunchRequest {
	return LaunchRequest{
		TaskID:    t.ID,
		Kind:      t.Kind,
		State:     t.State,
		RunID:     t.RunID,
		SessionID: t.ChildSessionID,
		Input:     input,
		Inherit:   t.Inherit,
	}
}

// Retry runs a failed task again from where it stopped: same id, same session,
// a new run — see spec §2.13.
func (m *Manager) Retry(ctx context.Context, taskID string) (*Info, error) {
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// The parent's spawn lock, as in Spawn: count then claim is a read-then-write.
	defer m.lockParent(t.ParentSessionID)()
	// Re-read under the lock: the row may have moved since the first read.
	if t, err = m.cfg.Store.Get(ctx, taskID); err != nil {
		return nil, err
	}
	if rerr := m.notRetryable(t); rerr != nil {
		// The refusal carries the task's state.
		return infoFrom(t, ""), rerr
	}

	live, err := m.cfg.Store.ListNonTerminal(ctx, t.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting live tasks: %w", err)
	}
	if limit := m.maxConcurrentPerParent(); len(live) >= limit {
		// A retry takes a concurrency slot like a spawn — see spec §2.13.
		return infoFrom(t, ""), ErrTaskLimit{Limit: limit}
	}

	// Read before the claim clears the failure: it is the next attempt's reason.
	prompt := retryPrompt(t, m.cfg.SummaryLimit)
	runID := m.cfg.NewID()
	won, err := m.cfg.Store.RetryClaim(ctx, taskID, runID, m.cfg.MaxAttemptsPerTask)
	if err != nil {
		return nil, err
	}
	if !won {
		cur, gerr := m.cfg.Store.Get(ctx, taskID)
		if gerr != nil {
			return nil, gerr
		}
		if rerr := m.notRetryable(cur); rerr != nil {
			return infoFrom(cur, ""), rerr
		}
		return infoFrom(cur, ""), ErrRetryConflict
	}
	m.resetContinued(taskID)

	defer m.beginLaunch(runID)()
	// The row moved to runID under the claim; t is the pre-claim read.
	claimed := *t
	claimed.RunID = runID
	req := launchFor(&claimed, prompt)
	req.Retry = true
	if err := m.cfg.Launcher(ctx, req); err != nil {
		return m.retryLaunchFailed(ctx, t, runID, err)
	}

	// Reconcile with any stop that raced the launch; see settleLaunch.
	updated, err := m.settleLaunch(ctx, taskID, runID)
	if err != nil {
		return nil, err
	}
	// The card shows working again now, not at the end of the run.
	m.notifyUpdate(ctx, updated)
	return infoFrom(updated, ""), nil
}

// MaxAttempts is the configured ceiling on a task's runs.
func (m *Manager) MaxAttempts() int { return m.cfg.MaxAttemptsPerTask }

// notRetryable explains why a task cannot be retried, or returns nil.
func (m *Manager) notRetryable(t *Task) error {
	if t.Status != StatusFailed {
		return ErrNotRetryable{Status: t.Status}
	}
	if t.AttemptNo() >= m.cfg.MaxAttemptsPerTask {
		return ErrRetryLimit{Limit: m.cfg.MaxAttemptsPerTask}
	}
	return nil
}

// retryLaunchFailed releases a claim whose run never started (the attempt
// rolls back) and reports the task with the error.
func (m *Manager) retryLaunchFailed(ctx context.Context, t *Task, runID string, cause error) (*Info, error) {
	// Detached: a launch usually fails because the teardown already cancelled ctx.
	ctx = context.WithoutCancel(ctx)
	full := "retry could not start: " + cause.Error()
	summary := truncateRunes(full, m.cfg.SummaryLimit)
	won, err := m.cfg.Store.ReleaseRetryClaim(ctx, t.ID, runID, summary, full)
	if err != nil {
		m.log.WarnContext(ctx, "failing a task whose retry never started",
			slog.String("task_id", t.ID), slog.String("error", err.Error()))
	}
	if won {
		m.finished(t.ID)
	}
	wrapped := fmt.Errorf("tasks: restarting task run: %w", cause)
	// The report carries the released values in hand, never a re-read — see spec §2.13.
	rel := *t
	rel.RunID, rel.Status, rel.Summary, rel.Result = runID, StatusFailed, summary, full
	rel.UpdatedAt = time.Now().UTC()
	cur, gerr := m.cfg.Store.Get(ctx, t.ID)
	if gerr == nil {
		m.notifyUpdate(ctx, cur)
	} else if won {
		m.notifyUpdate(ctx, &rel)
	}
	if won {
		m.finishedTask(ctx, &rel)
	}
	if gerr != nil {
		return nil, wrapped
	}
	return infoFrom(cur, ""), wrapped
}

// retryPrompt tells a retried run why it woke up; the session already holds
// the task's prompt and the failed attempt.
func retryPrompt(t *Task, limit int) string {
	reason := truncateRunes(strings.TrimSpace(cmp.Or(t.Result, t.Summary)), limit)
	if reason == "" {
		reason = "no reason was recorded"
	}
	return "A previous attempt at this task failed: " + reason + ". " +
		"The conversation above is the progress made so far. Review it and continue the task to " +
		"completion; avoid repeating work that already succeeded."
}

// ModelHasResult settles the wake-up debt of a finished task whose result the
// MODEL has in hand, bound to the attempt and status the caller read; a REST
// path whose result goes to a person must not call it — see spec §2.13.
func (m *Manager) ModelHasResult(ctx context.Context, info *Info) {
	if info == nil || !info.Status.Terminal() {
		return
	}
	t, err := m.cfg.Store.Get(ctx, info.TaskID)
	if err != nil {
		return
	}
	if t.AttemptNo() != info.Attempt || !t.Status.Terminal() {
		return
	}
	m.resultDelivered(ctx, t)
}

// resultDelivered tells the host the parent already has this result.
func (m *Manager) resultDelivered(ctx context.Context, t *Task) {
	if m.cfg.OnResultDelivered != nil {
		m.cfg.OnResultDelivered(ctx, t)
	}
}

// finishedTask tells the host a terminal state was claimed here.
func (m *Manager) finishedTask(ctx context.Context, t *Task) {
	if m.cfg.OnFinished != nil {
		m.cfg.OnFinished(ctx, t)
	}
}

// beginLaunch registers a run being launched and returns the release to defer.
func (m *Manager) beginLaunch(runID string) (release func()) {
	m.launchMu.Lock()
	if m.launching == nil {
		m.launching = map[string]bool{}
	}
	m.launching[runID] = false
	m.launchMu.Unlock()
	return func() {
		m.launchMu.Lock()
		delete(m.launching, runID)
		m.launchMu.Unlock()
	}
}

// noteRunReported records that the host reported runID ended, for a launch
// still settling.
func (m *Manager) noteRunReported(runID string) {
	if runID == "" {
		return
	}
	m.launchMu.Lock()
	if _, launching := m.launching[runID]; launching {
		m.launching[runID] = true
	}
	m.launchMu.Unlock()
}

// runReported reports whether the host has spoken about a run still being launched.
func (m *Manager) runReported(runID string) bool {
	m.launchMu.Lock()
	defer m.launchMu.Unlock()
	return m.launching[runID]
}

// settleLaunch re-reads the row after a launch and cancels the run when a
// terminator landed between the claim and the launch — see spec §2.13.
func (m *Manager) settleLaunch(ctx context.Context, taskID, runID string) (*Task, error) {
	// Detached: the teardown this cleans up after is what cancelled ctx.
	ctx = context.WithoutCancel(ctx)
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.RunID == runID && !t.Status.Terminal() {
		return t, nil
	}
	// An ending the host reported is this run's own — see spec §2.13.
	if m.runReported(runID) {
		return t, nil
	}
	if m.cfg.Stopper == nil {
		m.log.WarnContext(ctx, "a run outlived the task that started it, and there is no Stopper to cancel it",
			slog.String("task_id", taskID), slog.String("run_id", runID), slog.String("status", string(t.Status)))
		return t, nil
	}
	if _, serr := m.cfg.Stopper(ctx, runID, false); serr != nil {
		m.log.WarnContext(ctx, "stopping a run its task no longer owns",
			slog.String("task_id", taskID), slog.String("run_id", runID), slog.String("error", serr.Error()))
	}
	return t, nil
}

func (m *Manager) cleanupSession(ctx context.Context, id string) {
	if err := m.cfg.Sessions.Delete(ctx, id); err != nil {
		m.log.WarnContext(ctx, "orphan task session cleanup",
			slog.String("session_id", id), slog.String("error", err.Error()))
	}
}

// List reports a parent session's tasks, newest first; it settles no wake-up debt.
func (m *Manager) List(ctx context.Context, parentSessionID string) ([]*Info, error) {
	rows, err := m.cfg.Store.ListByParent(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	infos := make([]*Info, 0, len(rows))
	for i := range rows {
		infos = append(infos, infoFrom(&rows[i], ""))
	}
	return infos, nil
}

// Status reports a task, waiting up to wait for it to finish; a terminal
// status read here is delivered — see spec §2.13.
func (m *Manager) Status(ctx context.Context, taskID string, wait time.Duration) (*Info, error) {
	if wait > m.cfg.MaxStatusWait {
		wait = m.cfg.MaxStatusWait
	}
	deadline := time.Now().Add(wait)
	for {
		t, err := m.cfg.Store.Get(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if t.Status.Terminal() {
			// The row in hand is what the model reads: no re-read.
			m.resultDelivered(ctx, t)
			return infoFrom(t, ""), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return infoFrom(t, ""), nil
		}
		if !m.awaitFinish(ctx, taskID, remaining) {
			// Cancelled or timed out: report the row as it is.
			t, err := m.cfg.Store.Get(ctx, taskID)
			if err != nil {
				return nil, err
			}
			return infoFrom(t, ""), nil
		}
	}
}

// Stop cancels a task, chasing one retry: a task reopened between the read and
// the claim is stopped on its new attempt — see spec §2.13.
func (m *Manager) Stop(ctx context.Context, taskID string, graceful bool) (*Info, error) {
	for pass := range 2 {
		t, err := m.cfg.Store.Get(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if t.Status.Terminal() {
			if pass > 0 {
				// The first pass lost the CAS to an ending, which stands.
				return infoFrom(t, ""), nil
			}
			return infoFrom(t, ""), ErrAlreadyFinal{Status: t.Status}
		}

		verdict, err := m.stopAttempt(ctx, t, graceful, pass == 1)
		if err != nil {
			return nil, err
		}
		switch verdict {
		case stopDeferred:
			return infoFrom(t, ""), nil
		case stopClaimed:
			updated, err := m.cfg.Store.Get(ctx, taskID)
			if err != nil {
				return nil, err
			}
			m.notifyUpdate(ctx, updated)
			return infoFrom(updated, ""), nil
		case stopRunEnded:
			// The outcome is on its way to the row: wait for it — see spec §2.13.
			m.awaitSettled(ctx, taskID, t.RunID)
		case stopRetried:
			// A retry moved the task to a new run: go round against that attempt.
		}
	}
	// Both passes lost their claim: report the task as it stands.
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return infoFrom(t, ""), nil
}

// stopVerdict is what one stop attempt did; the zero value is no verdict.
type stopVerdict int

const (
	// stopClaimed: this call recorded the ending.
	stopClaimed stopVerdict = iota + 1
	// stopDeferred: the run took a graceful stop and will record its own ending.
	stopDeferred
	// stopRetried: the claim lost to a retry that moved the task to a new attempt.
	stopRetried
	// stopRunEnded: the run was already over; its outcome may still be on its way.
	stopRunEnded
)

// stopSettleWait bounds a stop's wait for a finished run's outcome to reach the row.
const stopSettleWait = 2 * time.Second

// awaitSettled waits, boundedly, for a finished run's outcome to reach the row
// (terminal, or moved on from runID) — see spec §2.13.
func (m *Manager) awaitSettled(ctx context.Context, taskID, runID string) {
	deadline := time.Now().Add(stopSettleWait)
	for time.Now().Before(deadline) {
		t, err := m.cfg.Store.Get(ctx, taskID)
		if err != nil || t.Status.Terminal() || t.RunID != runID {
			return
		}
		if !m.awaitFinish(ctx, taskID, time.Until(deadline)) {
			return // the caller went away
		}
	}
}

// stopAttempt cancels the one attempt t names and reports what it did; on the
// last pass a run the host says already finished is ended, not waited for.
func (m *Manager) stopAttempt(ctx context.Context, t *Task, graceful, last bool) (stopVerdict, error) {
	// A paused task is claimed first; a working one has its run cancelled first
	// — see spec §2.13.
	paused := t.Status == StatusInputRequired

	// No Stopper, or a failed one, reads as StopUnknownRun: this call records
	// the ending.
	stopRun := func() StopOutcome {
		if m.cfg.Stopper == nil {
			return StopUnknownRun
		}
		out, serr := m.cfg.Stopper(ctx, t.RunID, graceful)
		if serr != nil {
			m.log.WarnContext(ctx, "stopping task run",
				slog.String("task_id", t.ID), slog.String("error", serr.Error()))
			return StopUnknownRun
		}
		return out
	}
	if !paused {
		// StopAfterTurn and StopAlreadyFinished leave the ending to the run's own
		// report — see spec §2.13.
		switch stopRun() {
		case StopAfterTurn:
			return stopDeferred, nil
		case StopAlreadyFinished:
			// Also what a stop hears after a retry landed: go round again — see
			// decisions §5.54.
			if !last {
				return stopRunEnded, nil
			}
			// Last pass: the wait has happened, so the outcome is lost, not
			// late — see spec §2.13.
		case StopUnknownRun, StopCancelled:
		}
	}

	reason := "stopped"
	if paused {
		reason = "stopped while awaiting approval"
	}
	won, err := m.cfg.Store.Finalize(ctx, t.ID, t.RunID, StatusCancelled, reason, "", nil)
	if err != nil {
		return 0, err
	}
	if paused {
		// After the claim: the host discards the approval only once this call owns it.
		stopRun()
	} else if won {
		// Told again now the ending is ours: a mid-launch stop reaches nothing
		// — see spec §2.13.
		stopRun()
	}
	if won {
		// A cancellation is delivered, not finished, with the claimed values in
		// hand — see spec §2.13.
		done := *t
		done.Status, done.Summary, done.UpdatedAt = StatusCancelled, reason, time.Now().UTC()
		m.resultDelivered(ctx, &done)
		m.finished(t.ID)
		return stopClaimed, nil
	}
	return stopRetried, nil
}

// RunOutcome is what a finished run reports.
type RunOutcome struct {
	// RunID names the attempt that finished; empty means whichever the row
	// names. A host that can identify its runs sets it — see spec §2.13.
	RunID string
	// Status is the run's terminal state as the host sees it.
	Status Status
	// Text is the run's final output.
	Text string
	// Err is the failure message, when it failed.
	Err string
	// GracefulStop reports that the run finished because a stop asked it to,
	// after its turn.
	GracefulStop bool
}

// ownedBy verifies taskID belongs to parentSessionID; a foreign task reads as
// ErrNotFound.
func (m *Manager) ownedBy(ctx context.Context, parentSessionID, taskID string) error {
	if parentSessionID == "" {
		return fmt.Errorf("task tools: no session in the run context")
	}
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if t == nil || t.ParentSessionID != parentSessionID {
		return ErrNotFound
	}
	return nil
}

// OnRunFinished is the single entry point for advancing task state: a host
// calls it when any run ends, and a run of no task's session is ignored.
func (m *Manager) OnRunFinished(ctx context.Context, sessionID string, out RunOutcome) {
	task, err := m.cfg.Store.ByChildSession(ctx, sessionID)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && task == nil):
		return // not a task session
	case err != nil:
		// A failed lookup is not "not a task session" — see spec §2.13.
		m.log.ErrorContext(ctx, "resolving finished run's task; terminal state NOT recorded",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}

	// The attempt this outcome names, or the row's for a host that does not
	// identify runs.
	runID := cmp.Or(out.RunID, task.RunID)

	status := out.Status
	full := strings.TrimSpace(out.Text)
	if status == StatusFailed && full == "" && out.Err != "" {
		full = out.Err
	}
	summary := truncateRunes(full, m.cfg.SummaryLimit)

	// A clean finish under a graceful stop is a cancellation, not a completion.
	if status == StatusCompleted && out.GracefulStop {
		status = StatusCancelled
		summary = cmp.Or(summary, "stopped after the current turn")
	}

	if status == StatusInputRequired {
		// Not terminal; the resumed run lands back here. Bound to this attempt
		// — see spec §2.13.
		if err := m.cfg.Store.MarkInputRequired(ctx, task.ID, runID); err != nil {
			m.log.WarnContext(ctx, "marking task input_required",
				slog.String("task_id", task.ID), slog.String("error", err.Error()))
		}
		if t, err := m.cfg.Store.Get(ctx, task.ID); err == nil {
			m.notifyUpdate(ctx, t)
		}
		return
	}
	if !status.Terminal() {
		return
	}
	// Recorded before the claim, whoever wins it: a settling launch reads this.
	m.noteRunReported(runID)

	// Continue is asked only for the current attempt's named run, still working
	// — see spec §2.13.
	var finalState json.RawMessage
	consult := m.cfg.Continue != nil && status != StatusCancelled && !out.GracefulStop && task.RunID == runID && task.Status == StatusWorking
	if consult && out.RunID == "" {
		m.log.WarnContext(ctx, "task outcome without a run id: Continue not consulted; a job of several runs needs run identity",
			slog.String("task_id", task.ID))
		consult = false
	}
	if consult {
		cont, cerr := m.cfg.Continue(ctx, task, out)
		switch {
		case cerr != nil:
			status = StatusFailed
			full = cerr.Error()
			summary = truncateRunes(full, m.cfg.SummaryLimit)
		case cont != nil && cont.Input == "":
			// The job ends here, its final State written with the ending.
			finalState = cont.State
			if cont.Err != nil {
				status = StatusFailed
				full = cont.Err.Error()
				summary = truncateRunes(full, m.cfg.SummaryLimit)
			}
		case cont != nil && m.continuations(task.ID) >= m.cfg.MaxContinuations:
			// Another run past the ceiling ends the task failed — see spec §2.13.
			status = StatusFailed
			full = fmt.Sprintf("stopped after %d runs: the task's continuation ceiling (%d) was reached", m.continuations(task.ID)+1, m.cfg.MaxContinuations)
			summary = truncateRunes(full, m.cfg.SummaryLimit)
		case cont != nil:
			aerr := m.continueTask(ctx, task, runID, cont)
			if aerr == nil {
				return
			}
			// Not written or not won: finalized failed on the run that ended —
			// see spec §2.13.
			status = StatusFailed
			full = "could not advance to the next run: " + aerr.Error()
			summary = truncateRunes(full, m.cfg.SummaryLimit)
		}
	}

	won, err := m.cfg.Store.Finalize(ctx, task.ID, runID, status, summary, full, finalState)
	if err != nil {
		m.log.WarnContext(ctx, "finalizing task",
			slog.String("task_id", task.ID), slog.String("error", err.Error()))
		return
	}
	if !won {
		// Another finalizer owned the transition; its state stands.
		return
	}
	m.finished(task.ID)
	// The report is the claimed snapshot, never a re-read; the re-read below
	// only freshens the card — see spec §2.13.
	done := *task
	done.RunID, done.Status, done.Summary, done.Result = runID, status, summary, full
	if finalState != nil {
		done.State = finalState
	}
	done.UpdatedAt = time.Now().UTC()
	if t, gerr := m.cfg.Store.Get(ctx, task.ID); gerr == nil {
		m.notifyUpdate(ctx, t)
	} else {
		m.notifyUpdate(ctx, &done)
	}
	// A cancellation is delivered, not finished — see spec §2.13.
	if status == StatusCancelled {
		m.resultDelivered(ctx, &done)
		return
	}
	m.finishedTask(ctx, &done)
}

// errAdvanceLost is an Advance that found the row no longer working on the run
// that ended.
var errAdvanceLost = errors.New("the task was moved before the next run could start")

// continueTask claims the transition to the run Continue asked for, then
// launches; an error only for a claim not written or not won (errAdvanceLost).
func (m *Manager) continueTask(ctx context.Context, t *Task, fromRunID string, cont *Continuation) error {
	// Detached, as every launch is: the finishing run's teardown may have
	// cancelled ctx.
	ctx = context.WithoutCancel(ctx)
	nextRunID := m.cfg.NewID()
	won, err := m.cfg.Store.Advance(ctx, t.ID, fromRunID, nextRunID, cont.State)
	if err != nil {
		return err
	}
	if !won {
		return errAdvanceLost
	}
	m.noteContinued(t.ID)
	next := *t
	next.RunID = nextRunID
	if cont.State != nil { // nil keeps the recorded state, as Advance does
		next.State = cont.State
	}
	defer m.beginLaunch(nextRunID)()
	if lerr := m.cfg.Launcher(ctx, launchFor(&next, cont.Input)); lerr != nil {
		m.launchFailed(ctx, &next, "could not start the next run: "+lerr.Error())
		return nil
	}
	updated, err := m.settleLaunch(ctx, t.ID, nextRunID)
	if err != nil {
		m.log.WarnContext(ctx, "reading an advanced task", slog.String("task_id", t.ID), slog.String("error", err.Error()))
		return nil
	}
	m.notifyUpdate(ctx, updated)
	return nil
}

// launchFailed ends a task whose next run never started, on a detached
// context as retryLaunchFailed is.
func (m *Manager) launchFailed(ctx context.Context, t *Task, reason string) {
	ctx = context.WithoutCancel(ctx)
	summary := truncateRunes(reason, m.cfg.SummaryLimit)
	won, err := m.cfg.Store.Finalize(ctx, t.ID, t.RunID, StatusFailed, summary, reason, nil)
	if err != nil {
		m.log.WarnContext(ctx, "failing a task whose next run never started",
			slog.String("task_id", t.ID), slog.String("error", err.Error()))
		return
	}
	if !won {
		return
	}
	m.finished(t.ID)
	done := *t
	done.Status, done.Summary, done.Result = StatusFailed, summary, reason
	done.UpdatedAt = time.Now().UTC()
	if cur, gerr := m.cfg.Store.Get(ctx, t.ID); gerr == nil {
		m.notifyUpdate(ctx, cur)
	} else {
		m.notifyUpdate(ctx, &done)
	}
	m.finishedTask(ctx, &done)
}

// FailOrphans fails every task still recorded as working and reports each
// through OnFinished; it runs before the host accepts requests — see spec §2.13.
func (m *Manager) FailOrphans(ctx context.Context) error {
	orphans, err := m.cfg.Store.FailOrphans(ctx)
	if err != nil {
		return fmt.Errorf("tasks: failing orphaned tasks: %w", err)
	}
	if len(orphans) > 0 {
		m.log.InfoContext(ctx, "failed tasks orphaned by a restart", slog.Int("count", len(orphans)))
	}
	for i := range orphans {
		m.finishedTask(ctx, &orphans[i])
	}
	return nil
}

// StopTree cancels every non-terminal task of a session, for a teardown; the
// caller must already have blocked new runs on the session.
func (m *Manager) StopTree(ctx context.Context, sessionID string) error {
	live, err := m.cfg.Store.ListNonTerminal(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("tasks: listing live tasks: %w", err)
	}
	var errs []error
	for i := range live {
		if _, err := m.Stop(ctx, live[i].ID, false); err != nil {
			if _, ok := errors.AsType[ErrAlreadyFinal](err); ok {
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// notifyUpdate reports a task's changed state to the host.
func (m *Manager) notifyUpdate(ctx context.Context, t *Task) {
	if m.cfg.OnTaskUpdate != nil && t != nil {
		m.cfg.OnTaskUpdate(ctx, t)
	}
}

// statusPollInterval backstops the wake-up signal. See awaitFinish.
const statusPollInterval = 250 * time.Millisecond

// awaitFinish blocks until the task finishes, the timeout elapses or ctx ends,
// and reports whether to look again; the poll covers writes by another process.
func (m *Manager) awaitFinish(ctx context.Context, taskID string, timeout time.Duration) bool {
	ch := make(chan struct{})
	m.mu.Lock()
	m.waiters[taskID] = append(m.waiters[taskID], ch)
	m.mu.Unlock()
	defer m.dropWaiter(taskID, ch)

	poll := min(timeout, statusPollInterval)
	timer := time.NewTimer(poll)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		// Not necessarily finished — the caller re-reads and decides.
		return true
	case <-ctx.Done():
		return false
	}
}

// finished wakes everyone waiting on a task.
func (m *Manager) finished(taskID string) {
	m.mu.Lock()
	chans := m.waiters[taskID]
	delete(m.waiters, taskID)
	delete(m.continued, taskID)
	m.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}

// continuations is how many runs Continue has chained under the task so far.
func (m *Manager) continuations(taskID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.continued[taskID]
}

// noteContinued counts one more chained run for the task.
func (m *Manager) noteContinued(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.continued == nil {
		m.continued = map[string]int{}
	}
	m.continued[taskID]++
}

// resetContinued starts the count over at a retry — see spec §2.13.
func (m *Manager) resetContinued(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.continued, taskID)
}

func (m *Manager) dropWaiter(taskID string, ch chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rest := m.waiters[taskID][:0]
	for _, c := range m.waiters[taskID] {
		if c != ch {
			rest = append(rest, c)
		}
	}
	if len(rest) == 0 {
		delete(m.waiters, taskID)
		return
	}
	m.waiters[taskID] = rest
}

// truncateRunes caps s at n runes, never splitting a multi-byte character.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
