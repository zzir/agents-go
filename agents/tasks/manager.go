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
	NotifyFormatter        func([]Task) string

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

	// waiters wakes task_status callers when a task finishes, so a wait costs
	// one blocked goroutine rather than a polling loop.
	mu      sync.Mutex
	waiters map[string][]chan struct{}

	// spawning serializes Spawn per parent session. Counting live tasks and
	// creating the next one is a read-then-write, and the calls that race it
	// are the ordinary case: several spawn_task calls in one model response
	// execute concurrently, so without this they all see room and all create.
	//
	// Entries are reference-counted and removed when the last holder releases,
	// the same shape memory.FileSession uses for its per-path locks. A map
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
		Inherit:         spec.Inherit,
		Status:          StatusWorking,
	}
	if err := m.cfg.Store.Create(ctx, task); err != nil {
		m.cleanupSession(cleanupCtx, childID)
		return nil, fmt.Errorf("tasks: creating task: %w", err)
	}

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

	m.notifyUpdate(ctx, task)
	return infoFrom(task, spec.DisplayName), nil
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
			if err := m.cfg.Store.ConsumeNotify(ctx, taskID); err != nil {
				m.log.WarnContext(ctx, "consuming task notification",
					slog.String("task_id", taskID), slog.String("error", err.Error()))
			}
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
func (m *Manager) Stop(ctx context.Context, taskID string, graceful bool) (*Info, error) {
	t, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.Status.Terminal() {
		return infoFrom(t, ""), ErrAlreadyFinal{Status: t.Status}
	}

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

	// stopRun reports whether the run actually took the stop. Only then can
	// this call leave the terminal state to the run: with no Stopper, or one
	// that failed, nothing is going to finish the task, and returning success
	// would leave it working forever.
	stopRun := func() bool {
		if m.cfg.Stopper == nil {
			return false
		}
		if err := m.cfg.Stopper.Stop(ctx, t.RunID, graceful); err != nil {
			m.log.WarnContext(ctx, "stopping task run",
				slog.String("task_id", taskID), slog.String("error", err.Error()))
			return false
		}
		return true
	}
	if !paused {
		accepted := stopRun()
		if graceful && accepted {
			// The run finishes its turn and reports through OnRunFinished,
			// which records the cancellation. Finalizing here would race it.
			return infoFrom(t, ""), nil
		}
		// Not accepted: fall through and finalize here, which is what
		// Config.Stopper promises when there is no Stopper to interrupt with.
	}

	reason := "stopped"
	if paused {
		reason = "stopped while awaiting approval"
	}
	won, err := m.cfg.Store.Finalize(ctx, taskID, StatusCancelled, reason, "")
	if err != nil {
		return nil, err
	}
	if paused {
		// Told after the claim, so the host discards the approval only once
		// this call owns the transition.
		stopRun()
	}
	if won {
		// A cancellation never wakes the parent: the user initiated it, the UI
		// already shows it, and a wake-up run would only restate it.
		if err := m.cfg.Store.ConsumeNotify(ctx, taskID); err != nil {
			m.log.WarnContext(ctx, "consuming cancelled task notification",
				slog.String("task_id", taskID), slog.String("error", err.Error()))
		}
		m.finished(taskID)
	}
	updated, err := m.cfg.Store.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	m.notifyUpdate(ctx, updated)
	return infoFrom(updated, ""), nil
}

// RunOutcome is what a finished run reports.
type RunOutcome struct {
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

	won, err := m.cfg.Store.Finalize(ctx, task.ID, status, summary, full)
	if err != nil {
		m.log.WarnContext(ctx, "finalizing task",
			slog.String("task_id", task.ID), slog.String("error", err.Error()))
		return
	}
	if !won {
		// Another finalizer owned the transition — a stop, or a startup sweep.
		// Its state stands.
		return
	}
	if status == StatusCancelled {
		if err := m.cfg.Store.ConsumeNotify(ctx, task.ID); err != nil {
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
		if err := m.cfg.Store.MarkNotifyDelivered(ctx, pending[i].ID); err != nil {
			m.log.WarnContext(ctx, "marking task notification delivered",
				slog.String("task_id", pending[i].ID), slog.String("error", err.Error()))
		}
	}
}

// Recover reconciles after a restart: tasks recorded as running can never
// progress (their run died with the process), so they are failed, and every
// parent then owed a wake-up is drained.
func (m *Manager) Recover(ctx context.Context) error {
	n, err := m.cfg.Store.FailOrphans(ctx)
	if err != nil {
		return fmt.Errorf("tasks: failing orphaned tasks: %w", err)
	}
	if n > 0 {
		m.log.InfoContext(ctx, "failed tasks orphaned by a restart", slog.Int64("count", n))
	}
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
