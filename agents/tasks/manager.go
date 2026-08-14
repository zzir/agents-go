package tasks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// Defaults chosen to fail safe rather than to be generous.
const (
	// DefaultMaxConcurrentPerParent bounds tasks in flight for one parent.
	DefaultMaxConcurrentPerParent = 6
	// DefaultSummaryLimit is how much of a result reaches the notification and
	// the UI card, in runes.
	DefaultSummaryLimit = 300
	// DefaultMaxStatusWait bounds task_status's server-side wait.
	DefaultMaxStatusWait = 120 * time.Second
	// DefaultMaxDepth is how many task hops are allowed. A task spawned from an
	// ordinary session is depth 1, so the default of 1 means a task cannot
	// spawn tasks.
	DefaultMaxDepth = 1
	// DefaultMaxAttemptsPerTask bounds the runs one task may have, the original
	// included.
	DefaultMaxAttemptsPerTask = 3
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

// ErrRetryConflict reports a retry that lost its claim to another writer — a
// concurrent stop, or another process's retry — between the lock-guarded read
// and the compare-and-set. Trying again is the remedy; hosts map it to 409,
// not 500.
var ErrRetryConflict = errors.New("tasks: another writer claimed this task first; try again")

// Config configures a Manager. Store, Sessions, Resolver and Launcher are
// required.
type Config struct {
	Store    Store
	Sessions session.Repo
	Resolver AgentResolver
	Launcher Launcher
	// Stopper cancels a running task. Without one, Stop still finalizes the
	// row but cannot interrupt the run.
	Stopper Stopper
	// MaxConcurrentPerParent caps a parent session's live tasks. One Manager
	// enforces it exactly; several Managers over one Store can each admit up to
	// the cap, so run one Manager per parent or enforce the ceiling above them.
	MaxConcurrentPerParent int
	SummaryLimit           int
	MaxStatusWait          time.Duration
	MaxDepth               int
	// MaxAttemptsPerTask bounds Retry: how many runs one task may have, the
	// original included. Zero uses DefaultMaxAttemptsPerTask; 1 disables
	// retrying.
	MaxAttemptsPerTask int

	// NewID mints task, run and session ids. Nil uses a built-in generator.
	NewID func() string
	// Logger receives the Manager's own records; nil is silent.
	Logger *slog.Logger
	// OnTaskUpdate, when set, is called whenever a task's public state changes,
	// so a host can update the UI card for the spawn call that started it —
	// long after the spawning turn ended.
	OnTaskUpdate func(ctx context.Context, t *Task)
	// OnFinished, when set, is called once per task that reaches a terminal
	// state under THIS manager's claim. The parent has not heard the result:
	// delivering it is the host's business — a task that finished while its
	// parent was busy, paused or restarting cannot simply be announced — so the
	// Manager reports the fact and keeps no debt of its own. t is the claimed
	// terminal snapshot, built from the finalize's own values rather than a
	// re-read: by the time the hook runs, a retry may have moved the row on.
	OnFinished func(ctx context.Context, t *Task)
	// OnResultDelivered, when set, is called when the result reached the parent
	// some other way: the MODEL pulled it in-turn. A host that recorded
	// something to deliver drops it here.
	OnResultDelivered func(ctx context.Context, t *Task)

	// ExtraLiveCount, when set, adds background work the Manager does not track
	// — a host's OWN kind, e.g. workflow executions — to the per-parent live
	// count, so ONE cap governs every kind of background work rather than each
	// counting only its own. Called under the parent's spawn lock during Spawn.
	// It returns a plain int: a count that cannot be read must not wrongly BLOCK
	// a spawn, so the host returns 0 on any error.
	ExtraLiveCount func(ctx context.Context, parentSessionID string) int
}

// Manager owns the task lifecycle.
type Manager struct {
	cfg Config
	log *slog.Logger

	// waiters wakes task_status callers the moment this Manager finalizes a
	// task; awaitFinish also polls, since another process can be the writer.
	mu      sync.Mutex
	waiters map[string][]chan struct{}

	// launching holds the runs whose launch has not settled yet, and whether
	// the host has since reported one of them finishing. One entry per launch
	// in flight, so it is bounded by concurrent spawns rather than by history.
	launchMu  sync.Mutex
	launching map[string]bool

	// spawning serializes Spawn per parent session: counting live tasks and
	// creating the next is a read-then-write that concurrent spawn_task calls
	// would all pass. Entries are reference-counted and removed when the last
	// holder releases, so the map does not grow without bound.
	spawnMu  sync.Mutex
	spawning map[string]*parentSpawnLock
}

// parentSpawnLock is one entry in Manager.spawning: the per-parent mutex plus
// the number of current holders and waiters, managed under spawnMu.
type parentSpawnLock struct {
	mu   sync.Mutex
	refs int
}

// lockParent blocks until the spawn lock for parentSessionID is held and
// returns the release func, which must be called exactly once. Different
// parents proceed in parallel; the same parent is mutually exclusive.
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

// New returns a Manager. It panics on a configuration that cannot work, since
// that is a programming error rather than a runtime condition.
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
	if cfg.MaxConcurrentPerParent <= 0 {
		cfg.MaxConcurrentPerParent = DefaultMaxConcurrentPerParent
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
	if cfg.NewID == nil {
		cfg.NewID = newID
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg, log: log.With(slog.String("component", "tasks")), waiters: map[string][]chan struct{}{}}
}

// Meta describes a session's role in the task system.
type Meta struct {
	TaskID          string
	Label           string
	ParentSessionID string
	Depth           int
}

// MetaFor reports whether a session is a task's own session.
//
// A host uses it to decide whether to attach the task tools: a task run at the
// depth limit must not get them, which bounds recursion.
//
// A store failure is returned rather than folded into "not a task": the two
// answers withhold or hand out the task tools, so a failed lookup must not read
// as the permissive one.
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

// Spawn starts a background task for parentSessionID.
//
// It returns as soon as the run is launched: the parent gets a task id and
// carries on.
func (m *Manager) Spawn(ctx context.Context, req SpawnRequest) (*Info, error) {
	if req.ParentSessionID == "" {
		return nil, errors.New("tasks: Spawn requires a parent session")
	}

	// Depth first: a task at the limit must not even resolve an agent, or a
	// misconfigured host could be made to do work on the way to refusing.
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

	// Hold the parent's spawn lock across the count and the create: they are
	// one decision.
	defer m.lockParent(req.ParentSessionID)()

	// Check the cap BEFORE creating anything, so an over-cap spawn fails clean
	// rather than being rolled back.
	live, err := m.cfg.Store.ListNonTerminal(ctx, req.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting live tasks: %w", err)
	}
	// Count the host's other background work (workflows) against the same cap,
	// so N workflows leave room for fewer tasks rather than a full N more.
	total := len(live)
	if m.cfg.ExtraLiveCount != nil {
		total += m.cfg.ExtraLiveCount(ctx, req.ParentSessionID)
	}
	if total >= m.cfg.MaxConcurrentPerParent {
		return nil, ErrTaskLimit{Limit: m.cfg.MaxConcurrentPerParent}
	}

	spec, err := m.cfg.Resolver(ctx, req.ParentSessionID, req.AgentName)
	if err != nil {
		return nil, fmt.Errorf("tasks: resolving agent %q: %w", req.AgentName, err)
	}

	label := req.Label
	if label == "" {
		label = truncateRunes(req.Input, 60)
	}

	// Cleanup runs on a context detached from the caller's: a parent
	// cancellation racing the spawn would otherwise kill ctx mid-rollback and
	// leave a ghost child session.
	cleanupCtx := context.WithoutCancel(ctx)

	// The id is minted here rather than read back, so a failed read cannot
	// leave a session nothing refers to.
	childID := m.cfg.NewID()
	if _, err := m.cfg.Sessions.Create(ctx, session.CreateOptions{
		ID:    childID,
		Title: "task: " + label,
		// Hidden: a task's transcript is not a conversation the user started.
		Hidden: true,
	}); err != nil {
		return nil, fmt.Errorf("tasks: creating task session: %w", err)
	}

	task := &Task{
		ID:              m.cfg.NewID(),
		RunID:           m.cfg.NewID(),
		Label:           label,
		ParentSessionID: req.ParentSessionID,
		ParentRunID:     req.ParentRunID,
		ToolCallID:      req.ToolCallID,
		ChildSessionID:  childID,
		Depth:           depth,
		Attempt:         1,
		Inherit:         spec.Inherit,
		Status:          StatusWorking,
	}
	if err := m.cfg.Store.Create(ctx, task); err != nil {
		m.cleanupSession(cleanupCtx, childID)
		return nil, fmt.Errorf("tasks: creating task: %w", err)
	}

	defer m.beginLaunch(task.RunID)()
	if err := m.cfg.Launcher(ctx, LaunchRequest{
		RunID:     task.RunID,
		SessionID: childID,
		Input:     req.Input,
		Inherit:   spec.Inherit,
	}); err != nil {
		// The run never started, so unwind rather than leaving a failed husk.
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
	// ParentRunID and ToolCallID identify the spawning turn, so a UI card can
	// be updated when the task finishes.
	ParentRunID string
	ToolCallID  string
}

// Retry runs a failed task again, from where it stopped.
//
// The task keeps its id, its session and everything that session holds; only
// the run is new — a failed task has often done most of its work. Resuming is
// sound because persistence stops at a boundary that never leaves a tool call
// without its output, so the tail of a failed attempt is valid model input.
func (m *Manager) Retry(ctx context.Context, taskID string) (*Info, error) {
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// The parent's spawn lock, for the same reason Spawn holds it: counting
	// then claiming is a read-then-write two retries would both pass.
	defer m.lockParent(t.ParentSessionID)()
	// Re-read under the lock — the row could have finished, been stopped or
	// been retried between the first read and here.
	if t, err = m.cfg.Store.Get(ctx, taskID); err != nil {
		return nil, err
	}
	if rerr := m.notRetryable(t); rerr != nil {
		// The task's own state travels with the refusal, so a caller can show
		// what it is, not only why.
		return infoFrom(t, ""), rerr
	}

	live, err := m.cfg.Store.ListNonTerminal(ctx, t.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting live tasks: %w", err)
	}
	if len(live) >= m.cfg.MaxConcurrentPerParent {
		// A retry queues behind the same ceiling a spawn does; exempting it
		// would make retry the way around the cap.
		return infoFrom(t, ""), ErrTaskLimit{Limit: m.cfg.MaxConcurrentPerParent}
	}

	// Read the failure BEFORE the claim clears it: it is what tells the next
	// attempt why it is starting over.
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

	defer m.beginLaunch(runID)()
	if err := m.cfg.Launcher(ctx, LaunchRequest{
		RunID:     runID,
		SessionID: t.ChildSessionID,
		Input:     prompt,
		Inherit:   t.Inherit,
	}); err != nil {
		return m.retryLaunchFailed(ctx, t, runID, err)
	}

	// Reconcile with any stop that raced the launch; see settleLaunch.
	updated, err := m.settleLaunch(ctx, taskID, runID)
	if err != nil {
		return nil, err
	}
	// Say now that it is working again, rather than at the end of a run that
	// may take minutes.
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

// retryLaunchFailed puts a claimed task back to failed after its new run never
// started, and reports the task as it now stands alongside the error.
//
// ReleaseRetryClaim rolls the attempt count back: the run never launched, and
// Attempt counts the runs a task has actually had, so an infrastructure failure
// must not spend the retry ceiling.
func (m *Manager) retryLaunchFailed(ctx context.Context, t *Task, runID string, cause error) (*Info, error) {
	// Detached for the same reason as settleLaunch: a launch usually fails
	// because the session is being torn down, which has already cancelled ctx.
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
	// The report carries the released values in hand — a re-read can fail or
	// race a second retry, and the ending must not go unreported. The release
	// rolled the attempt back to the pre-claim row's, so t's count is right.
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

// retryPrompt is what a retried run is asked to do. The session already holds
// everything the failed attempt did, so this is not the task's prompt again —
// it is the reason the run woke up.
func retryPrompt(t *Task, limit int) string {
	reason := truncateRunes(strings.TrimSpace(cmp.Or(t.Result, t.Summary)), limit)
	if reason == "" {
		reason = "no reason was recorded"
	}
	return "A previous attempt at this task failed: " + reason + ". " +
		"The conversation above is the progress made so far. Review it and continue the task to " +
		"completion; avoid repeating work that already succeeded."
}

// modelHasResult cancels the wake-up debt of a finished task whose result the
// MODEL now has in hand. It is the model that matters, not the caller: a REST
// path whose result goes to a person must NOT come through here, or the model
// never gets its wake-up.
//
// Bound to the attempt and the terminal status the caller read: a retry between
// the decision and this write makes the pending debt a different attempt's, and
// a result that landed after the call was decided is one the model has not seen.
func (m *Manager) modelHasResult(ctx context.Context, info *Info) {
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

// resultDelivered tells the host the parent already has this result, so
// whatever it recorded to deliver can be dropped.
func (m *Manager) resultDelivered(ctx context.Context, t *Task) {
	if m.cfg.OnResultDelivered != nil {
		m.cfg.OnResultDelivered(ctx, t)
	}
}

// finishedTask tells the host a terminal state was claimed here, so it can
// arrange for the parent to hear about it.
func (m *Manager) finishedTask(ctx context.Context, t *Task) {
	if m.cfg.OnFinished != nil {
		m.cfg.OnFinished(ctx, t)
	}
}

// beginLaunch registers a run about to be launched, and returns the release
// its caller must defer.
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

// noteRunReported records that a run has ENDED and the host said so — what
// tells a launch still settling that a terminal row is its own doing, not an
// orphan's.
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

// runReported reports whether the host has spoken about a run still being
// launched.
func (m *Manager) runReported(runID string) bool {
	m.launchMu.Lock()
	defer m.launchMu.Unlock()
	return m.launching[runID]
}

// settleLaunch reconciles a task with the run just started for it, and returns
// the task as it now stands.
//
// Starting a run is two steps — claim the row, then tell the host — and a
// terminator landing between them acts on an attempt the host never heard of:
// its Stopper reaches nothing and the launch proceeds. settleLaunch closes that
// half for terminators that never speak to the host (an approval reaper, a
// restart sweep); a stop closes its own by telling the host again.
func (m *Manager) settleLaunch(ctx context.Context, taskID, runID string) (*Task, error) {
	// Detached, for the reason Spawn's rollback is: the teardown this cleans up
	// after is the same event that cancels ctx, and a cancelled read here would
	// leave the run executing.
	ctx = context.WithoutCancel(ctx)
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.RunID == runID && !t.Status.Terminal() {
		return t, nil
	}
	// The row is terminal or on a later attempt. If the host reported this run,
	// that ending is its OWN — a task that finished before the launch call
	// returned — and cancelling it would rewrite an outcome the host's clients
	// already saw.
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

// Status reports a task, optionally waiting for it to finish.
//
// Reaching a terminal status here CONSUMES the wake-up debt: the model has the
// result in hand, so waking it later to repeat the news would burn a turn.
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
			// The row is in hand and IS what the model reads, so the bound
			// write needs no re-read.
			m.resultDelivered(ctx, t)
			return infoFrom(t, ""), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return infoFrom(t, ""), nil
		}
		if !m.awaitFinish(ctx, taskID, remaining) {
			// Context cancelled, or the wait ran out; report what it is now.
			t, err := m.cfg.Store.Get(ctx, taskID)
			if err != nil {
				return nil, err
			}
			return infoFrom(t, ""), nil
		}
	}
}

// Stop cancels a task.
//
// A stop names the TASK, so it chases one retry: a task reopened between the
// read and the claim is stopped on its new attempt. One extra pass, not a loop
// — an unbounded chase would never end against a retry storm.
func (m *Manager) Stop(ctx context.Context, taskID string, graceful bool) (*Info, error) {
	for pass := range 2 {
		t, err := m.cfg.Store.Get(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if t.Status.Terminal() {
			if pass > 0 {
				// The first pass lost the CAS and this one finds the task
				// finished: whoever won recorded its ending, which stands.
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
			// The run is over and its outcome is on its way to the row. Wait
			// for it, so the next pass reports the real ending instead of
			// racing it — and, if none arrives, records one of its own.
			m.awaitSettled(ctx, taskID)
		case stopRetried:
			// A retry reopened the task on a new run between the read and the
			// claim. Go round at once, against that attempt.
		}
	}
	// The last pass lost its claim too: the task is on an attempt this call
	// never saw, so report it as it stands.
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return infoFrom(t, ""), nil
}

// stopVerdict is what one attempt at stopping a task did; the four answers
// steer the pass after it. They start at one, so the zero value is no verdict —
// what a failed attempt returns beside its error.
type stopVerdict int

const (
	// stopClaimed: this call recorded the ending.
	stopClaimed stopVerdict = iota + 1
	// stopDeferred: the run took a graceful stop and will record its own.
	stopDeferred
	// stopRetried: the claim lost, so a retry has moved the task to an attempt
	// this pass never saw.
	stopRetried
	// stopRunEnded: the run was already over and nothing was claimed — its
	// outcome may still be on its way to the row.
	stopRunEnded
)

// stopSettleWait bounds how long a stop waits for a finished run's outcome to
// reach the row before recording an ending of its own.
const stopSettleWait = 2 * time.Second

// awaitSettled waits, briefly, for a finished run's outcome to reach the row —
// the difference between a stop that reports a task's real ending and one that
// overwrites it. The bound covers an outcome that was LOST rather than late.
func (m *Manager) awaitSettled(ctx context.Context, taskID string) {
	deadline := time.Now().Add(stopSettleWait)
	for time.Now().Before(deadline) {
		t, err := m.cfg.Store.Get(ctx, taskID)
		if err != nil || t.Status.Terminal() {
			return
		}
		if !m.awaitFinish(ctx, taskID, time.Until(deadline)) {
			return // the caller went away
		}
	}
}

// stopAttempt cancels the one attempt t names and reports what it did. last
// says this is the final pass, after which nothing else will chase the task —
// which is what turns a run the host reports as already finished from
// something to wait for into something to end.
func (m *Manager) stopAttempt(ctx context.Context, t *Task, graceful, last bool) (stopVerdict, error) {
	// A paused task has no running goroutine, so finalizing IS the exclusive
	// claim: a concurrent approval's ReclaimWorking (input_required → working)
	// and this Finalize (non-terminal → cancelled) cannot both win. Claim it
	// before telling the Stopper, or an approve slipping in between would
	// resume a task this call has already reported as cancelled.
	//
	// A working task is the other way round: cancel the run first, because
	// otherwise its own completion wins the CAS and records a success for
	// something the user asked to stop.
	paused := t.Status == StatusInputRequired

	// stopRun reports what the host did. With no Stopper, or one that failed,
	// nothing was cancelled — which reads as StopUnknownRun, so this call
	// records the ending itself.
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
		// Two answers leave the ending to the run's own report: a graceful
		// wind-down, or a run that had already finished. Recording a
		// cancellation over the second would overwrite a real outcome — and
		// cost a failure its retry. Everything else falls through, including a
		// still-launching run the host has never heard of.
		switch stopRun() {
		case StopAfterTurn:
			return stopDeferred, nil
		case StopAlreadyFinished:
			// "That run is over" is also what a stop hears when a retry landed
			// between its read and this call. Reporting the run ended sends Stop
			// round again — to find the ending that run recorded, or chase the
			// attempt that replaced it — rather than writing a cancellation over
			// an outcome already on its way.
			if !last {
				return stopRunEnded, nil
			}
			// Except on the last pass: waiting has already happened, the row
			// still names this attempt, so the outcome was lost, not late.
			// Falling through to claim is safe — the CAS below is bound to this
			// attempt and a non-terminal row, so a real outcome landing first
			// still wins.
		case StopUnknownRun, StopCancelled:
		}
	}

	reason := "stopped"
	if paused {
		reason = "stopped while awaiting approval"
	}
	won, err := m.cfg.Store.Finalize(ctx, t.ID, t.RunID, StatusCancelled, reason, "")
	if err != nil {
		return 0, err
	}
	if paused {
		// Told after the claim, so the host discards the approval only once
		// this call owns the transition.
		stopRun()
	} else if won {
		// Told AGAIN, now that the ending is ours: the first call may have gone
		// out while the run was still launching, reaching nothing. It exists by
		// now and nothing else will stop it. A Stopper must tolerate cancelling
		// a run that already ended.
		stopRun()
	}
	if won {
		// A cancellation never wakes the parent: the user initiated it, the UI
		// already shows it, and a wake-up run would only restate it. Reported
		// with the claimed values in hand — the pre-claim row still says working.
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
	// RunID names the attempt that finished. A host that can identify its runs
	// should set it, or an outcome could overwrite the attempt that replaced
	// this one. Empty means "whichever attempt the row names".
	RunID string
	// Status is the run's terminal state as the host sees it.
	Status Status
	// Text is the run's final output.
	Text string
	// Err is the failure message, when it failed — without it the task would
	// only ever say "failed" with no why.
	Err string
	// GracefulStop reports that the run finished because someone asked it to
	// stop after the current turn.
	GracefulStop bool
}

// ownedBy verifies taskID belongs to parentSessionID. A task id that leaked
// into another conversation must read as nonexistent there, not as a handle: a
// foreign task_status would consume the rightful parent's wake-up debt, a
// foreign task_stop cancel work the caller does not own.
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

// OnRunFinished is the single entry point for advancing task state. A host
// calls it when any run ends.
//
// It handles the parent's sessions too, not only task sessions: a parent that
// was busy while a task finished has debts waiting, and its own run boundary is
// where they can finally be paid.
func (m *Manager) OnRunFinished(ctx context.Context, sessionID string, out RunOutcome) {
	task, err := m.cfg.Store.ByChildSession(ctx, sessionID)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && task == nil):
		return // not a task session
	case err != nil:
		// A failure to LOOK is not "not a task session": proceeding would drop
		// the terminal state on the floor — the task stuck working, its parent
		// never woken — until a restart's FailOrphans declares it dead. Refuse
		// loudly instead.
		m.log.ErrorContext(ctx, "resolving finished run's task; terminal state NOT recorded",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}

	// The attempt this outcome belongs to: what the host reported, or — for a
	// host that does not identify runs — whichever one the row names.
	runID := cmp.Or(out.RunID, task.RunID)

	status := out.Status
	full := strings.TrimSpace(out.Text)
	if status == StatusFailed && full == "" && out.Err != "" {
		full = out.Err
	}
	summary := truncateRunes(full, m.cfg.SummaryLimit)

	// A clean finish under a graceful stop IS a cancellation. Recording it as
	// a completion would tell the user their stop did nothing.
	if status == StatusCompleted && out.GracefulStop {
		status = StatusCancelled
		summary = cmp.Or(summary, "stopped after the current turn")
	}

	if status == StatusInputRequired {
		// Not terminal: the approval flow surfaces it and the resumed run lands
		// back here with a final status. Bound to this outcome's attempt: an
		// approval that outlived its attempt must not pause the one that
		// replaced it.
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
	// The run ENDED and the host said so — what a launch still settling needs
	// before reading a terminal row as its own doing. Recorded regardless of
	// who wins the transition below: the fact is about the run, not the row.
	m.noteRunReported(runID)

	won, err := m.cfg.Store.Finalize(ctx, task.ID, runID, status, summary, full)
	if err != nil {
		m.log.WarnContext(ctx, "finalizing task",
			slog.String("task_id", task.ID), slog.String("error", err.Error()))
		return
	}
	if !won {
		// Another finalizer owned the transition — a stop, a startup sweep, or
		// a retry that has already moved the task past this attempt. Its state
		// stands.
		return
	}
	m.finished(task.ID)
	// The report carries the claimed values IN HAND, never a re-read: between
	// the win and a Get, a retry can move the row past this attempt (leaving a
	// non-terminal row with the failure cleared), and a failed read must not
	// cost the parent the report. The re-read below only freshens the UI card.
	done := *task
	done.RunID, done.Status, done.Summary, done.Result = runID, status, summary, full
	done.UpdatedAt = time.Now().UTC()
	if t, gerr := m.cfg.Store.Get(ctx, task.ID); gerr == nil {
		m.notifyUpdate(ctx, t)
	} else {
		m.notifyUpdate(ctx, &done)
	}
	// A cancellation is reported as DELIVERED, not finished: the user did it,
	// the UI already shows it, and a turn restating it would only repeat them.
	if status == StatusCancelled {
		m.resultDelivered(ctx, &done)
		return
	}
	m.finishedTask(ctx, &done)
}

// Recover reconciles after a restart: tasks recorded as running can never
// progress (their run died with the process), so they are failed and reported
// through OnFinished, which is where a host arranges to tell their parents.
func (m *Manager) Recover(ctx context.Context) error {
	return m.FailOrphans(ctx)
}

// FailOrphans is the first half of Recover: every task still recorded as
// running is failed, which owes its parent a wake-up.
//
// It must complete BEFORE the host accepts a retry: the sweep fails every
// working row there is, so a retry that got in first would have its fresh run
// declared dead and its parent woken with a failure that never happened.
func (m *Manager) FailOrphans(ctx context.Context) error {
	orphans, err := m.cfg.Store.FailOrphans(ctx)
	if err != nil {
		return fmt.Errorf("tasks: failing orphaned tasks: %w", err)
	}
	if len(orphans) > 0 {
		m.log.InfoContext(ctx, "failed tasks orphaned by a restart", slog.Int("count", len(orphans)))
	}
	// Each parent still has to hear about it — the whole point of a durable
	// debt is that a restart is exactly when one is owed.
	for i := range orphans {
		m.finishedTask(ctx, &orphans[i])
	}
	return nil
}

// StopTree cancels every non-terminal task of a session, for a teardown.
//
// The caller must have already blocked new runs on the session — otherwise a
// task finishing mid-teardown drains a notification that starts a run outliving
// the cascade about to delete everything.
func (m *Manager) StopTree(ctx context.Context, sessionID string) error {
	live, err := m.cfg.Store.ListNonTerminal(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("tasks: listing live tasks: %w", err)
	}
	var errs []error
	for i := range live {
		if _, err := m.Stop(ctx, live[i].ID, false); err != nil {
			var final ErrAlreadyFinal
			if errors.As(err, &final) {
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
// and reports whether the caller should look again.
//
// The signal makes it prompt; the poll makes it correct — this Manager is not
// the only writer a task row has, so a waiter listening only for its OWN
// transitions could sit out the timeout while the answer sat in the store.
func (m *Manager) awaitFinish(ctx context.Context, taskID string, timeout time.Duration) bool {
	ch := make(chan struct{})
	m.mu.Lock()
	m.waiters[taskID] = append(m.waiters[taskID], ch)
	m.mu.Unlock()
	defer m.dropWaiter(taskID, ch)

	poll := statusPollInterval
	if timeout < poll {
		poll = timeout
	}
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
	m.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
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

// truncateRunes caps s at n runes, cutting on a rune boundary so a multi-byte
// character is never split into invalid UTF-8.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
