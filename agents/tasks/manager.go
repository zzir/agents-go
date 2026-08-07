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

// Defaults chosen to fail safe rather than to be generous.
const (
	// DefaultMaxConcurrentPerParent bounds tasks in flight for one parent. A
	// model told it can delegate will delegate.
	DefaultMaxConcurrentPerParent = 6
	// DefaultSummaryLimit is how much of a result reaches the notification and
	// the UI card, in runes.
	DefaultSummaryLimit = 300
	// DefaultMaxStatusWait bounds task_status's server-side wait: long enough
	// that one call outlives most tasks, short enough that a stuck task returns
	// control to the model.
	DefaultMaxStatusWait = 120 * time.Second
	// DefaultMaxDepth is how many task hops are allowed. A task spawned from
	// an ordinary session is depth 1, so the default of 1 means a task cannot
	// spawn tasks — recursion is a real use case, but not a default.
	DefaultMaxDepth = 1
	// DefaultMaxAttemptsPerTask bounds the runs one task may have, the original
	// included. A model that can retry without limit will keep retrying a
	// failure that was never going to succeed.
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

// ErrNotRetryable reports a retry of a task that is not failed. It is a
// separate answer from ErrAlreadyFinal, whose problem is the opposite one: a
// retry REQUIRES a finished task, and only the one kind of ending it can
// resume from.
type ErrNotRetryable struct{ Status Status }

func (e ErrNotRetryable) Error() string {
	return fmt.Sprintf("tasks: cannot retry a task that is %s; only a failed task can be retried", e.Status)
}

// ErrRetryLimit reports a task that has used every attempt it is allowed.
type ErrRetryLimit struct{ Limit int }

func (e ErrRetryLimit) Error() string {
	return fmt.Sprintf("tasks: this task has already used its %d attempts; start a new task instead", e.Limit)
}

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
	// Guard decides whether a parent may be woken. Nil never wakes: a Manager
	// that cannot tell whether waking is safe must not guess.
	Guard WakeGuard

	// MaxConcurrentPerParent caps a parent session's live tasks. One Manager
	// enforces it exactly: Spawn holds a per-parent lock across counting and
	// creating. Several Managers over one Store (separate processes) can each
	// admit up to the cap, because the Store has no conditional insert to
	// arbitrate between them — run one Manager per parent, or enforce the
	// ceiling above them.
	MaxConcurrentPerParent int
	SummaryLimit           int
	MaxStatusWait          time.Duration
	MaxDepth               int
	// MaxAttemptsPerTask bounds Retry: how many runs one task may have, the
	// original included. Zero uses DefaultMaxAttemptsPerTask; 1 disables
	// retrying.
	MaxAttemptsPerTask int
	NotifyFormatter    func([]Task) string

	// NewID mints task, run and session ids. Nil uses a built-in generator.
	NewID func() string
	// Logger receives the Manager's own records; nil is silent.
	Logger *slog.Logger
	// OnTaskUpdate, when set, is called whenever a task's public state changes,
	// so a host can update the UI card for the spawn call that started it.
	//
	// It is a callback rather than a return value because the change happens
	// long after the spawning turn ended — which is the whole difficulty this
	// subsystem exists to handle.
	OnTaskUpdate func(ctx context.Context, t *Task)
}

// Manager owns the task lifecycle.
type Manager struct {
	cfg Config
	log *slog.Logger

	// waiters wakes task_status callers the moment this Manager finalizes a
	// task. It makes the wait prompt rather than authoritative: another process
	// can be the writer, so awaitFinish also re-reads the store on a short
	// timer.
	mu      sync.Mutex
	waiters map[string][]chan struct{}

	// launching holds the runs whose launch has not settled yet, and whether
	// the host has since reported one of them finishing.
	//
	// settleLaunch cannot get that from the task row: a run that finished the
	// moment it started and a run something ended while the host could not
	// reach it leave the SAME row — terminal, on that run id. Reading the
	// first as the second cancels a run that is already over, which on a host
	// that keeps finished runs around rewrites their outcome for everyone
	// watching. Only the Manager knows, because OnRunFinished comes through it.
	//
	// It holds one entry per launch in flight, so it is bounded by concurrent
	// spawns rather than by history.
	launchMu  sync.Mutex
	launching map[string]bool

	// spawning serializes Spawn per parent session. Counting live tasks and
	// creating the next one is a read-then-write, and the calls that race it
	// are the ordinary case: several spawn_task calls in one model response
	// execute concurrently, so without this they all see room and all create.
	//
	// Entries are reference-counted and removed when the last holder releases,
	// the same shape filesession.Store uses for its per-path locks. A map
	// keyed on session id that only ever grows is a leak in a server: sessions
	// churn, and a parent that does not exist, one over the cap and one whose
	// launch failed each leave an entry behind just for asking.
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

// spawnLockCount reports how many per-parent locks are live. Test-only: the
// count is zero whenever no Spawn is in flight, which is the property the
// reference counting exists to keep.
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
	if cfg.NotifyFormatter == nil {
		cfg.NotifyFormatter = DefaultNotifyFormatter
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
// depth limit must not get them, which is how recursion is bounded.
//
// A store failure is returned rather than reported as "not a task". The two
// answers lead to opposite actions — one withholds the task tools, the other
// hands them out — so collapsing a failed lookup into the permissive one turns
// a transient query error into a way past MaxDepth. A caller with nothing
// better to do must refuse, not proceed.
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
// carries on, which is the entire point of a task.
func (m *Manager) Spawn(ctx context.Context, req SpawnRequest) (*Info, error) {
	if req.ParentSessionID == "" {
		return nil, errors.New("tasks: Spawn requires a parent session")
	}

	// Depth first: a task at the limit must not even resolve an agent, or a
	// misconfigured host could be made to do work on the way to refusing.
	//
	// A task spawned from an ordinary session is depth 1, so MaxDepth reads as
	// "how many task hops deep may this go" — the default of 1 means a task
	// cannot spawn tasks.
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
	// one decision, and the concurrent spawns this has to survive are the
	// normal case rather than an edge one.
	defer m.lockParent(req.ParentSessionID)()

	// Check the cap BEFORE creating anything, so an over-cap spawn fails clean
	// rather than being rolled back.
	live, err := m.cfg.Store.ListNonTerminal(ctx, req.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting live tasks: %w", err)
	}
	if len(live) >= m.cfg.MaxConcurrentPerParent {
		return nil, ErrTaskLimit{Limit: m.cfg.MaxConcurrentPerParent}
	}

	spec, err := m.cfg.Resolver.Resolve(ctx, req.ParentSessionID, req.AgentName)
	if err != nil {
		return nil, fmt.Errorf("tasks: resolving agent %q: %w", req.AgentName, err)
	}

	label := req.Label
	if label == "" {
		label = truncateRunes(req.Input, 60)
	}

	// Cleanup runs on a context detached from the caller's. Spawn is invoked
	// from inside the parent run, so a parent cancellation racing the spawn
	// would kill ctx mid-rollback and leave a ghost child session that nothing
	// owns and the user can still open.
	cleanupCtx := context.WithoutCancel(ctx)

	// The id is minted here rather than read back, so the task row can be
	// written without a second round trip and a failed read cannot leave a
	// session nothing refers to.
	childID := m.cfg.NewID()
	if _, err := m.cfg.Sessions.Create(ctx, session.CreateOptions{
		ID:    childID,
		Title: "task: " + label,
		// Hidden: a task's transcript is not a conversation the user started,
		// and listing it would bury the ones they did.
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
	if err := m.cfg.Launcher.Launch(ctx, LaunchRequest{
		RunID:     task.RunID,
		SessionID: childID,
		Input:     req.Input,
		Inherit:   spec.Inherit,
	}); err != nil {
		// The run never started, so unwind rather than leaving a failed husk:
		// the tool error is the model's record of this attempt, and a row would
		// only clutter the task list on a retry.
		if delErr := m.cfg.Store.Delete(cleanupCtx, task.ID); delErr != nil {
			m.log.WarnContext(ctx, "unstarted task row cleanup", slog.String("task_id", task.ID),
				slog.String("error", delErr.Error()))
		}
		m.cleanupSession(cleanupCtx, childID)
		return nil, fmt.Errorf("tasks: starting task run: %w", err)
	}

	// The row was visible to a teardown from the moment it was created, and
	// the run was not: a StopTree in between cancelled a run the host could not
	// reach, and this launch would leave it executing.
	settled, err := m.settleLaunch(ctx, task.ID, task.RunID)
	if err != nil {
		return nil, err
	}
	m.notifyUpdate(ctx, settled)
	return m.infoOf(settled, spec.DisplayName), nil
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
// The task keeps its id, its session and everything that session already
// holds; only the run is new. That is the whole point: a failed task has often
// done most of its work, and the alternative a parent reaches for — spawning a
// fresh task with the same prompt — pays for all of it a second time.
//
// Resuming is sound because the session is: persistence stops at a boundary
// that never leaves a tool call without its output, so the tail of a failed
// attempt is valid model input.
func (m *Manager) Retry(ctx context.Context, taskID string) (*Info, error) {
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// The parent's spawn lock, for the same reason Spawn holds it: a retry
	// takes a concurrency slot back, and counting then claiming is a
	// read-then-write two retries would both pass.
	defer m.lockParent(t.ParentSessionID)()
	// Re-read under the lock — the row could have finished, been stopped or
	// been retried between the first read and here.
	if t, err = m.cfg.Store.Get(ctx, taskID); err != nil {
		return nil, err
	}
	if rerr := m.notRetryable(t); rerr != nil {
		// The task's own state travels with the refusal, so a caller can show
		// what it actually is instead of only why it said no.
		return m.infoOf(t, ""), rerr
	}

	live, err := m.cfg.Store.ListNonTerminal(ctx, t.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("tasks: counting live tasks: %w", err)
	}
	if len(live) >= m.cfg.MaxConcurrentPerParent {
		// A retry is a task coming back to life, so it queues behind the same
		// ceiling a spawn does; exempting it would make retry the way around
		// the cap.
		return m.infoOf(t, ""), ErrTaskLimit{Limit: m.cfg.MaxConcurrentPerParent}
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
			return m.infoOf(cur, ""), rerr
		}
		return m.infoOf(cur, ""), errors.New("tasks: another writer claimed this task first; try again")
	}

	defer m.beginLaunch(runID)()
	if err := m.cfg.Launcher.Launch(ctx, LaunchRequest{
		RunID:     runID,
		SessionID: t.ChildSessionID,
		Input:     prompt,
		Inherit:   t.Inherit,
	}); err != nil {
		return nil, m.retryLaunchFailed(ctx, taskID, runID, err)
	}

	// A stop that arrived while this run was being launched has already
	// recorded its ending — against a run the host could not reach. Report what
	// the task IS rather than the working state we claimed, and see the
	// orphaned run stopped.
	updated, err := m.settleLaunch(ctx, taskID, runID)
	if err != nil {
		return nil, err
	}
	// The card is showing a failed task with a failure on it. Say now that it
	// is working again, rather than at the end of a run that may take minutes.
	m.notifyUpdate(ctx, updated)
	return m.infoOf(updated, ""), nil
}

// Retryable reports whether a task's OWN state allows a retry: it failed, and
// it has attempts left. A host offering a retry asks rather than infers —
// "failed" alone does not answer it, since the ceiling is this Manager's
// configuration and a task that has used every attempt looks the same from
// outside.
//
// It deliberately says nothing about capacity. Retry also refuses when the
// parent is at its live-task ceiling, and that is a transient condition which
// can change between an offer being rendered and someone taking it — a
// precomputed answer would be wrong as often as it was right. Such a refusal
// is ErrTaskLimit, and it explains itself.
func (m *Manager) Retryable(status Status, attempt int) bool {
	return status == StatusFailed && max(attempt, 1) < m.cfg.MaxAttemptsPerTask
}

// MaxAttempts is the configured ceiling on a task's runs. A host that has to
// answer "would a retry be allowed" about state it holds itself — a UI
// tracking tasks live — takes the parameter rather than asking per task, so
// its answer moves with the state instead of lagging a round trip behind it.
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
// started, and returns the error to report.
//
// Unlike Spawn's rollback there is no row to remove: the task existed before
// this call and its history is real. What must not survive is the working
// status — nothing is going to advance it — nor a wake-up debt for news the
// caller is being told to its face.
func (m *Manager) retryLaunchFailed(ctx context.Context, taskID, runID string, cause error) error {
	full := "retry could not start: " + cause.Error()
	won, err := m.cfg.Store.Finalize(ctx, taskID, runID, StatusFailed,
		truncateRunes(full, m.cfg.SummaryLimit), full)
	if err != nil {
		m.log.WarnContext(ctx, "failing a task whose retry never started",
			slog.String("task_id", taskID), slog.String("error", err.Error()))
	}
	if won {
		// Only the winner consumes: on a loss the debt belongs to whoever
		// recorded the ending, and cancelling it would bury their news.
		if cerr := m.cfg.Store.ConsumeNotify(ctx, taskID, runID); cerr != nil {
			m.log.WarnContext(ctx, "consuming task notification",
				slog.String("task_id", taskID), slog.String("error", cerr.Error()))
		}
		m.finished(taskID)
	}
	if t, gerr := m.cfg.Store.Get(ctx, taskID); gerr == nil {
		m.notifyUpdate(ctx, t)
	}
	return fmt.Errorf("tasks: restarting task run: %w", cause)
}

// retryPrompt is what a retried run is asked to do.
//
// The session already holds everything the failed attempt did, so this is not
// the task's prompt again — it is the reason the run woke up. Naming the
// failure is what lets the model route around it instead of walking back into
// it, and saying "do not redo what worked" is what keeps a resumed run from
// starting over out of politeness.
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
// MODEL now has in hand.
//
// It is the model that matters, not the caller: waking a conversation to
// deliver news the model already read burns a turn to repeat itself. A person
// reading the same result over an HTTP response has been told nothing, so a
// REST path must NOT come through here — the model still needs its wake-up.
//
// Bound to the attempt just read: between that read and this write a retry can
// reopen the task, and consuming then would cancel the NEW attempt's debt on
// the strength of the old one's result.
// info is what the caller is ABOUT to hand the model, and the debt is
// cancelled only if that is genuinely the news: a task still shown as running
// owes its wake-up however the row reads by now, because a result that landed
// after the call was decided is a result the model has not seen. Consuming on
// the row instead swallowed exactly that — the model was told "working", the
// wake-up was cancelled, and the answer reached nobody.
//
// The attempt is checked for the same reason in the other direction: a retry
// between the decision and this write makes the pending debt a different
// attempt's, and that one is nobody's to cancel.
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
	m.consumeNotify(ctx, t)
}

// consumeNotify cancels a finished task's wake-up debt, bound to the attempt
// the caller read.
func (m *Manager) consumeNotify(ctx context.Context, t *Task) {
	if err := m.cfg.Store.ConsumeNotify(ctx, t.ID, t.RunID); err != nil {
		m.log.WarnContext(ctx, "consuming task notification",
			slog.String("task_id", t.ID), slog.String("error", err.Error()))
	}
}

// infoOf is the public view of a task. It is a method because retryability is
// the Manager's policy — the row knows its status and its attempt, not the
// ceiling they are measured against.
func (m *Manager) infoOf(t *Task, agent string) *Info {
	info := infoFrom(t, agent)
	info.Retryable = m.notRetryable(t) == nil
	return info
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

// noteRunReported records that the host has spoken about a run — which is
// proof it knows about it, and therefore that a stop would have reached it.
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
// terminator that lands between them acts on an attempt the host has never
// heard of: its Stopper call reaches nothing, and the launch goes ahead anyway.
// The result is a run executing for a task that is cancelled (or already on a
// later attempt), unstoppable, its own outcome unrecordable because the row it
// would finalize is no longer its own.
//
// A stop closes its half by telling the host again once the ending is
// unambiguously its own. This closes the other half, for the terminators that
// never speak to the host at all: an approval reaper, a restart sweep.
func (m *Manager) settleLaunch(ctx context.Context, taskID, runID string) (*Task, error) {
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.RunID == runID && !t.Status.Terminal() {
		return t, nil
	}
	// The row is terminal, or on a later attempt. If the host reported this
	// run, that terminal state is its OWN doing — a task that finished before
	// the launch call returned, which is ordinary and often instant when a run
	// fails its pre-flight. Cancelling then would be cancelling something that
	// already ended, and a host holding finished runs would rewrite the outcome
	// its clients just saw.
	if m.runReported(runID) {
		return t, nil
	}
	if m.cfg.Stopper == nil {
		m.log.WarnContext(ctx, "a run outlived the task that started it, and there is no Stopper to cancel it",
			slog.String("task_id", taskID), slog.String("run_id", runID), slog.String("status", string(t.Status)))
		return t, nil
	}
	if _, serr := m.cfg.Stopper.Stop(ctx, runID, false); serr != nil {
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
// The wait trades one blocked goroutine for the model's polling loop, which is
// a real token saving: without it a model asks "is it done" every turn. Reaching
// a terminal status here CONSUMES the wake-up debt — the model has the result
// in hand, and waking it later to repeat the news would burn a turn.
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
			// The row is in hand and it IS what the model is about to read, so
			// the bound write needs no re-read.
			m.consumeNotify(ctx, t)
			return m.infoOf(t, ""), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return m.infoOf(t, ""), nil
		}
		if !m.awaitFinish(ctx, taskID, remaining) {
			// Context cancelled, or the wait ran out; report what it is now.
			t, err := m.cfg.Store.Get(ctx, taskID)
			if err != nil {
				return nil, err
			}
			return m.infoOf(t, ""), nil
		}
	}
}

// Stop cancels a task.
//
// A stop names the TASK, so it chases one retry: a task that was reopened
// between the read and the claim is stopped on its new attempt rather than
// reported as still running. One extra pass, not a loop — the caller can ask
// again, and an unbounded chase never ends against a retry storm.
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
				return m.infoOf(t, ""), nil
			}
			return m.infoOf(t, ""), ErrAlreadyFinal{Status: t.Status}
		}

		early, won, err := m.stopAttempt(ctx, t, graceful)
		if err != nil {
			return nil, err
		}
		if early {
			return m.infoOf(t, ""), nil
		}
		if !won {
			// A retry reopened the task on a new run between the read and the
			// claim. Go round once against that attempt.
			continue
		}
		updated, err := m.cfg.Store.Get(ctx, taskID)
		if err != nil {
			return nil, err
		}
		m.notifyUpdate(ctx, updated)
		return m.infoOf(updated, ""), nil
	}
	// Two passes, two losses. Report the task as it stands rather than
	// insisting: it is live, and saying otherwise would be a lie the UI shows.
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return m.infoOf(t, ""), nil
}

// stopAttempt cancels the one attempt t names. early reports that the run took
// a graceful stop and will record the ending itself; won reports that this call
// owns the terminal transition.
func (m *Manager) stopAttempt(ctx context.Context, t *Task, graceful bool) (early, won bool, err error) {
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
	// nothing was told anything — which reads the same as a run the host has
	// never heard of, and leads to the same place: this call records the
	// ending itself rather than leaving a task working forever.
	stopRun := func() StopOutcome {
		if m.cfg.Stopper == nil {
			return StopUnknownRun
		}
		out, serr := m.cfg.Stopper.Stop(ctx, t.RunID, graceful)
		if serr != nil {
			m.log.WarnContext(ctx, "stopping task run",
				slog.String("task_id", t.ID), slog.String("error", serr.Error()))
			return StopUnknownRun
		}
		return out
	}
	if !paused {
		// Two answers mean the ending is not this call's to write, and both
		// leave it to the run's own report: it is winding up gracefully, or it
		// had already finished before the stop arrived. Recording a
		// cancellation over the second would overwrite a real outcome — and
		// for a failure, cost the task the retry it had earned.
		//
		// Everything else falls through, including a host that has never heard
		// of this run because it is still being launched: reading its "nothing
		// to do" as "it will take care of itself" is how a stop gets reported
		// as accepted while the task runs on.
		switch stopRun() {
		case StopAfterTurn, StopAlreadyFinished:
			return true, false, nil
		case StopUnknownRun, StopCancelled:
		}
	}

	reason := "stopped"
	if paused {
		reason = "stopped while awaiting approval"
	}
	won, err = m.cfg.Store.Finalize(ctx, t.ID, t.RunID, StatusCancelled, reason, "")
	if err != nil {
		return false, false, err
	}
	if paused {
		// Told after the claim, so the host discards the approval only once
		// this call owns the transition.
		stopRun()
	} else if won {
		// Told AGAIN, now that the ending is ours. The first call may have gone
		// out while the run was still being launched — a run the host had never
		// heard of, so the stop reached nothing and the launch went ahead. It
		// exists by now, and nothing else will ever stop it: its own outcome
		// cannot even be recorded against the row this call just finalized.
		// Asking a Stopper to cancel a run that already ended is something it
		// has to tolerate regardless.
		stopRun()
	}
	if won {
		// A cancellation never wakes the parent: the user initiated it, the UI
		// already shows it, and a wake-up run would only restate it.
		if cerr := m.cfg.Store.ConsumeNotify(ctx, t.ID, t.RunID); cerr != nil {
			m.log.WarnContext(ctx, "consuming cancelled task notification",
				slog.String("task_id", t.ID), slog.String("error", cerr.Error()))
		}
		m.finished(t.ID)
	}
	return false, won, nil
}

// RunOutcome is what a finished run reports.
type RunOutcome struct {
	// RunID names the attempt that finished. A host that can identify its runs
	// should set it: the task may have been retried since this run started, and
	// an outcome recorded against the attempt that REPLACED it would overwrite
	// a live run's row with a dead one's result. Empty means "whichever attempt
	// the row names" — the answer for a host with no run identity of its own,
	// and the behavior before retries existed.
	RunID string
	// Status is the run's terminal state as the host sees it.
	Status Status
	// Text is the run's final output.
	Text string
	// Err is the failure message, when it failed. A failed run's reason travels
	// here: without it the task would only ever say "failed" with no why.
	Err string
	// GracefulStop reports that the run finished because someone asked it to
	// stop after the current turn.
	GracefulStop bool
}

// ownedBy verifies taskID belongs to parentSessionID. A task id that leaked
// into another conversation must read as nonexistent there, not as a handle:
// spawn already refuses to act on a model-supplied session id for exactly this
// reason, and status/stop are the same boundary — a foreign task_status
// consumes the rightful parent's wake-up debt, and a foreign task_stop cancels
// work the caller does not own.
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
		// Not a task session: it may be a parent that was too busy to be woken.
		m.DrainPending(ctx, sessionID)
		return
	case err != nil:
		// A failure to LOOK is not "not a task session". Proceeding as if it
		// were silently drops the terminal state on the floor: the task stays
		// working forever, its concurrency slot held, its parent never woken —
		// until a restart's FailOrphans declares it dead. The check that
		// cannot be made refuses loudly instead; FailOrphans remains the
		// recovery of record.
		m.log.ErrorContext(ctx, "resolving finished run's task; terminal state NOT recorded",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}

	// The attempt this outcome belongs to: what the host reported, or — for a
	// host that does not identify runs — whichever one the row names.
	runID := cmp.Or(out.RunID, task.RunID)
	// The host has spoken about this run, so it knows the run exists — which is
	// exactly what a launch still settling needs to know before deciding the run
	// was orphaned. Recorded before any of the branching below, and regardless
	// of who wins a transition: the fact is about the run, not about the row.
	m.noteRunReported(runID)

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
		// back here with a final status.
		if err := m.cfg.Store.MarkInputRequired(ctx, task.ID); err != nil {
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
	if status == StatusCancelled {
		if err := m.cfg.Store.ConsumeNotify(ctx, task.ID, runID); err != nil {
			m.log.WarnContext(ctx, "consuming cancelled task notification",
				slog.String("task_id", task.ID), slog.String("error", err.Error()))
		}
	}
	m.finished(task.ID)
	if t, err := m.cfg.Store.Get(ctx, task.ID); err == nil {
		m.notifyUpdate(ctx, t)
	}
	m.DrainPending(ctx, task.ParentSessionID)
}

// DrainPending wakes a parent session with the results of every task that owes
// it one.
//
// Four guards must all pass. They are the accumulated answers to "when is
// waking wrong", and each one was a bug:
//
//   - the session is being deleted — a run started now outlives the cascade;
//   - the session already has a live run — the winner re-drains at its own
//     boundary, so the debt is not lost;
//   - the session is paused on a human decision — the human decides, and
//     waking races them;
//   - the guard itself errored — "cannot prove it is safe" is not permission.
//
// The first three are the host's to answer, through WakeGuard. The fourth is
// the guard's own contract.
func (m *Manager) DrainPending(ctx context.Context, parentSessionID string) {
	if parentSessionID == "" {
		return
	}
	if m.cfg.Guard == nil || !m.cfg.Guard.CanWake(ctx, parentSessionID) {
		return
	}
	pending, err := m.cfg.Store.ListPendingNotify(ctx, parentSessionID)
	if err != nil {
		m.log.WarnContext(ctx, "listing pending task notifications",
			slog.String("session_id", parentSessionID), slog.String("error", err.Error()))
		return
	}
	if len(pending) == 0 {
		return
	}

	// The wake-up runs under the configuration the SPAWNING run had, snapshotted
	// on the task. Resolving it fresh would use whatever the parent is
	// configured with now, which may be a different agent entirely.
	var inherit json.RawMessage
	for i := range pending {
		if len(pending[i].Inherit) > 0 {
			inherit = pending[i].Inherit
			break
		}
	}

	if err := m.cfg.Launcher.Launch(ctx, LaunchRequest{
		RunID:     m.cfg.NewID(),
		SessionID: parentSessionID,
		Input:     m.cfg.NotifyFormatter(pending),
		Inherit:   inherit,
		Wake:      true,
	}); err != nil {
		// Lost a race with a new user run: the debts stay pending and the
		// winner's boundary re-drains them.
		m.log.DebugContext(ctx, "task notification run did not start",
			slog.String("session_id", parentSessionID), slog.String("error", err.Error()))
		return
	}
	for i := range pending {
		// Bound to the attempt whose result this run carries: the Launch above
		// is long enough for a retry to reopen one of these tasks, and marking
		// THAT delivered would bury a wake-up nobody has heard.
		if err := m.cfg.Store.MarkNotifyDelivered(ctx, pending[i].ID, pending[i].RunID); err != nil {
			m.log.WarnContext(ctx, "marking task notification delivered",
				slog.String("task_id", pending[i].ID), slog.String("error", err.Error()))
		}
	}
}

// Recover reconciles after a restart: tasks recorded as running can never
// progress (their run died with the process), so they are failed, and every
// parent then owed a wake-up is drained.
//
// A host that can serve requests while this runs should call the two halves
// itself instead — see FailOrphans for the ordering that matters.
func (m *Manager) Recover(ctx context.Context) error {
	if err := m.FailOrphans(ctx); err != nil {
		return err
	}
	return m.DrainAllPending(ctx)
}

// FailOrphans is the first half of Recover: every task still recorded as
// running is failed, which owes its parent a wake-up.
//
// It must complete BEFORE the host accepts a retry. The sweep has no notion of
// a live run — it fails every working row there is — so a retry that got in
// first would have its fresh run declared dead, its parent woken with a failure
// that never happened, and the real result thrown away when the run finally
// lands. Nothing in the store can arbitrate that: the retry has already won its
// claim by the time the sweep looks.
func (m *Manager) FailOrphans(ctx context.Context) error {
	n, err := m.cfg.Store.FailOrphans(ctx)
	if err != nil {
		return fmt.Errorf("tasks: failing orphaned tasks: %w", err)
	}
	if n > 0 {
		m.log.InfoContext(ctx, "failed tasks orphaned by a restart", slog.Int64("count", n))
	}
	return nil
}

// DrainAllPending is the second half of Recover: every parent owed a wake-up
// is drained. It starts runs, so a host with its own startup ordering runs it
// once the rest of the machinery is in place.
func (m *Manager) DrainAllPending(ctx context.Context) error {
	parents, err := m.cfg.Store.PendingNotifyParents(ctx)
	if err != nil {
		return fmt.Errorf("tasks: listing notify-pending parents: %w", err)
	}
	for _, sid := range parents {
		m.DrainPending(ctx, sid)
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

// awaitFinish blocks until the task finishes, the timeout elapses or ctx ends.
// It reports whether the caller should look again.
//
// The signal makes it prompt; the poll makes it correct. This Manager is not
// the only writer a task row has — another process finalizes its own tasks, a
// startup sweep fails orphans, an operator resolves one by hand — and a waiter
// that only listened for its OWN transitions would sit out the full timeout
// while the answer sat in the store.
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
