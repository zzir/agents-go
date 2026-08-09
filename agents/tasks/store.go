package tasks

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// ErrNotFound is returned by Store lookups for an unknown task.
var ErrNotFound = errors.New("tasks: not found")

// Store persists tasks.
//
// It requires TRANSACTIONAL semantics, and that is a requirement rather than a
// preference: Finalize is a compare-and-set, and correctness depends on it.
// Two finalizers race routinely — a run completing while a stop is in flight,
// a startup sweep meeting a live run — and without the CAS both write, so a
// terminal state gets overwritten and the parent is woken twice or not at all.
//
// The SDK ships InMemoryStore; the sessions module ships a SQL one. There is
// deliberately no file-backed implementation: it could not offer the guarantee.
type Store interface {
	Create(ctx context.Context, t *Task) error
	Get(ctx context.Context, id string) (*Task, error)
	ByChildSession(ctx context.Context, sessionID string) (*Task, error)
	ListByParent(ctx context.Context, parentSessionID string) ([]Task, error)

	// Finalize records a terminal status, its result and the wake-up debt in
	// ONE atomic transition, and only while the task is still non-terminal AND
	// still on the attempt named by runID.
	//
	// won=false means another finalizer already owned the transition, or the
	// attempt being finalized is no longer the current one; the caller must do
	// nothing further. Writing the three parts separately would let task_status
	// observe a terminal task whose result is not there yet.
	//
	// The runID predicate is what RetryClaim costs: a task can leave a terminal
	// state now, so "the row is non-terminal" no longer identifies WHICH
	// attempt a finalizer observed. Without it, a stop that read the row before
	// a retry would cancel the new attempt — while its run keeps executing,
	// unkillable, its own result discarded for losing the CAS.
	Finalize(ctx context.Context, id, runID string, st Status, summary, result string) (won bool, err error)

	// RetryClaim reopens a failed task for another attempt, in one atomic
	// transition and only while the task is failed and under maxAttempts
	// (which counts the original run; <= 0 means no limit): status returns to
	// working, run_id becomes newRunID, attempt increments, and the previous
	// attempt's summary, result and wake-up debt are cleared — the debt because
	// this task is no longer finished, so nothing is owed until it is again.
	//
	// won=false means the row exists but could not be claimed: it is not
	// failed, it is out of attempts, or another retry won the race. A task that
	// does not exist is ErrNotFound, which is a different answer and must not
	// be collapsed into won=false.
	RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (won bool, err error)

	// ReleaseRetryClaim undoes a RetryClaim whose run never launched: status
	// back to failed, the attempt count back down, the launch failure recorded
	// as the task's summary/result, and the wake-up debt reopened (the parent
	// has not heard this ending). The attempt must roll back because Attempt's
	// contract is "the runs this task has HAD" — a launch that failed before
	// registering started nothing, and letting it stand would let a run of
	// infrastructure failures exhaust the retry ceiling without a single retry
	// executing.
	//
	// It applies only while runID is the current attempt and the row is still
	// working; won=false means another writer moved the task first and its
	// state stands.
	ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string) (won bool, err error)

	// MarkInputRequired flips working → input_required, only while runID is
	// the task's current attempt. Best-effort: a concurrent terminal
	// transition (or a newer attempt) wins.
	//
	// The runID predicate matches Finalize's, and for the same reason. These
	// two once ran unbound ("a non-terminal state can only belong to the
	// current attempt"), but an APPROVAL outlives the attempt that opened it:
	// persisted before the pause lands on the row, it can survive a crash, a
	// FailOrphans sweep and a retry — and then pause or cancel the attempt
	// that replaced its own. Every attempt-scoped writer names its attempt.
	MarkInputRequired(ctx context.Context, id, runID string) error
	// ReclaimWorking flips input_required → working when an approval resumes
	// the run, only while runID is the current attempt. false means the task
	// went terminal, was retried past this attempt, or is not paused — the
	// resume must be abandoned (and a stale approval discarded, not retried).
	ReclaimWorking(ctx context.Context, id, runID string) (bool, error)

	// ConsumeNotify cancels the wake-up debt: the model already has the result.
	// It applies only while runID is the task's current attempt — a consume
	// decided against the previous attempt must not swallow the debt of the
	// one that replaced it.
	ConsumeNotify(ctx context.Context, id, runID string) error
	// MarkNotifyDelivered records that a wake-up run carried the result, with
	// the same attempt bound as ConsumeNotify: a drain spans a Launch call, and
	// a retry can land inside it.
	MarkNotifyDelivered(ctx context.Context, id, runID string) error
	// ListPendingNotify returns the parent's tasks still owed a wake-up,
	// oldest first — the notification lists them in that order.
	ListPendingNotify(ctx context.Context, parentSessionID string) ([]Task, error)
	// PendingNotifyParents returns every session owed at least one wake-up.
	PendingNotifyParents(ctx context.Context) ([]string, error)

	// FailOrphans marks every still-working task failed, owing each parent a
	// wake-up. Called at startup: a task run does not survive a restart, so a
	// row left at working can never progress on its own.
	FailOrphans(ctx context.Context) (int64, error)
	// ListNonTerminal returns a parent's unfinished tasks, for a teardown that
	// must stop them.
	ListNonTerminal(ctx context.Context, parentSessionID string) ([]Task, error)

	Delete(ctx context.Context, id string) error
}

// InMemoryStore is a goroutine-safe Store for tests and single-process use.
//
// It takes one lock for every operation rather than a finer scheme, because
// the operations are short and the thing being protected is a state machine
// where a torn read is a wrong answer, not a slow one.
type InMemoryStore struct {
	mu    sync.Mutex
	tasks map[string]*Task
	order []string
}

// NewInMemoryStore returns an empty store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{tasks: map[string]*Task{}}
}

// Create implements Store.
func (s *InMemoryStore) Create(_ context.Context, t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		return errors.New("tasks: Create requires an ID")
	}
	if _, exists := s.tasks[t.ID]; exists {
		return errors.New("tasks: duplicate task id " + t.ID)
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	cp := *t
	s.tasks[t.ID] = &cp
	s.order = append(s.order, t.ID)
	return nil
}

// Get implements Store.
func (s *InMemoryStore) Get(_ context.Context, id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *t
	return &cp, nil
}

// ByChildSession implements Store.
func (s *InMemoryStore) ByChildSession(_ context.Context, sessionID string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		if t := s.tasks[id]; t != nil && t.ChildSessionID == sessionID {
			cp := *t
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// ListByParent implements Store, newest first — the order a task list shows.
func (s *InMemoryStore) ListByParent(_ context.Context, parentSessionID string) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	// Newest first, matching what a task list shows.
	for i := len(s.order) - 1; i >= 0; i-- {
		if t := s.tasks[s.order[i]]; t != nil && t.ParentSessionID == parentSessionID {
			out = append(out, *t)
		}
	}
	return out, nil
}

// Finalize implements Store. The whole transition happens under one lock, so a
// reader can never see a terminal task whose result has not landed.
func (s *InMemoryStore) Finalize(_ context.Context, id, runID string, st Status, summary, result string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status.Terminal() || t.RunID != runID {
		return false, nil
	}
	t.Status = st
	t.NotifyState = NotifyPending
	if summary != "" {
		t.Summary = summary
	}
	if result != "" {
		t.Result = result
	}
	t.UpdatedAt = time.Now().UTC()
	return true, nil
}

// RetryClaim implements Store.
func (s *InMemoryStore) RetryClaim(_ context.Context, id, newRunID string, maxAttempts int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status != StatusFailed || (maxAttempts > 0 && t.AttemptNo() >= maxAttempts) {
		return false, nil
	}
	t.Status = StatusWorking
	t.RunID = newRunID
	t.Attempt = t.AttemptNo() + 1
	// The previous attempt's account of itself, cleared: it describes a run
	// that is no longer this task's, and a card showing "failed: rate limited"
	// beside a working badge reads as a task failing right now.
	t.Summary, t.Result = "", ""
	// No longer finished, so nothing is owed. The next terminal state opens a
	// fresh debt.
	t.NotifyState = NotifyNone
	t.UpdatedAt = time.Now().UTC()
	return true, nil
}

// ReleaseRetryClaim implements Store.
func (s *InMemoryStore) ReleaseRetryClaim(_ context.Context, id, runID, summary, result string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status != StatusWorking || t.RunID != runID {
		return false, nil
	}
	t.Status = StatusFailed
	// The claim counted a run that never happened; AttemptNo() floors at 1, so
	// the rollback can never go below the original run.
	t.Attempt = max(t.AttemptNo()-1, 1)
	t.Summary, t.Result = summary, result
	t.NotifyState = NotifyPending
	t.UpdatedAt = time.Now().UTC()
	return true, nil
}

// MarkInputRequired implements Store.
func (s *InMemoryStore) MarkInputRequired(_ context.Context, id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok && t.Status == StatusWorking && t.RunID == runID {
		t.Status = StatusInputRequired
		t.UpdatedAt = time.Now().UTC()
	}
	return nil
}

// ReclaimWorking implements Store.
func (s *InMemoryStore) ReclaimWorking(_ context.Context, id, runID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status != StatusInputRequired || t.RunID != runID {
		return false, nil
	}
	t.Status = StatusWorking
	t.UpdatedAt = time.Now().UTC()
	return true, nil
}

// ConsumeNotify and MarkNotifyDelivered deliberately leave UpdatedAt alone. For
// a terminal task that column is when it FINISHED — created→updated is the
// duration a UI shows — and delivery can happen minutes later.
func (s *InMemoryStore) ConsumeNotify(_ context.Context, id, runID string) error {
	return s.setNotify(id, runID, NotifyConsumed)
}

// MarkNotifyDelivered implements Store.
func (s *InMemoryStore) MarkNotifyDelivered(_ context.Context, id, runID string) error {
	return s.setNotify(id, runID, NotifyDelivered)
}

func (s *InMemoryStore) setNotify(id, runID string, to NotifyState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok && t.NotifyState == NotifyPending && t.RunID == runID {
		t.NotifyState = to
	}
	return nil
}

// ListPendingNotify implements Store, oldest first.
func (s *InMemoryStore) ListPendingNotify(_ context.Context, parentSessionID string) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, id := range s.order {
		if t := s.tasks[id]; t != nil && t.ParentSessionID == parentSessionID && t.NotifyState == NotifyPending {
			out = append(out, *t)
		}
	}
	return out, nil
}

// PendingNotifyParents implements Store.
func (s *InMemoryStore) PendingNotifyParents(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, id := range s.order {
		t := s.tasks[id]
		if t == nil || t.NotifyState != NotifyPending || seen[t.ParentSessionID] {
			continue
		}
		seen[t.ParentSessionID] = true
		out = append(out, t.ParentSessionID)
	}
	return out, nil
}

// FailOrphans implements Store.
func (s *InMemoryStore) FailOrphans(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, id := range s.order {
		t := s.tasks[id]
		// input_required rows are kept: their pending approval persists and
		// resumes the run, so they are not orphans.
		if t == nil || t.Status != StatusWorking {
			continue
		}
		t.Status = StatusFailed
		t.Summary = "the process restarted while the task was running"
		// The failure is news the parent never heard, so it owes a wake-up.
		t.NotifyState = NotifyPending
		t.UpdatedAt = time.Now().UTC()
		n++
	}
	return n, nil
}

// ListNonTerminal implements Store.
func (s *InMemoryStore) ListNonTerminal(_ context.Context, parentSessionID string) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, id := range s.order {
		if t := s.tasks[id]; t != nil && t.ParentSessionID == parentSessionID && !t.Status.Terminal() {
			out = append(out, *t)
		}
	}
	return out, nil
}

// Delete implements Store.
func (s *InMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	if i := slices.Index(s.order, id); i >= 0 {
		s.order = slices.Delete(s.order, i, i+1)
	}
	return nil
}

var _ Store = (*InMemoryStore)(nil)
