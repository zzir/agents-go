package tasks

import (
	"context"
	"errors"
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
	// ONE atomic transition, and only while the task is still non-terminal.
	//
	// won=false means another finalizer already owned the transition; the
	// caller must do nothing further. Writing the three parts separately would
	// let task_status observe a terminal task whose result is not there yet.
	Finalize(ctx context.Context, id string, st Status, summary, result string) (won bool, err error)

	// MarkInputRequired flips working → input_required. Best-effort: a
	// concurrent terminal transition wins.
	MarkInputRequired(ctx context.Context, id string) error
	// ReclaimWorking flips input_required → working when an approval resumes
	// the run. false means the task went terminal meanwhile and the resume
	// must be abandoned.
	ReclaimWorking(ctx context.Context, id string) (bool, error)

	// ConsumeNotify cancels the wake-up debt: the model already has the result.
	ConsumeNotify(ctx context.Context, id string) error
	// MarkNotifyDelivered records that a wake-up run carried the result.
	MarkNotifyDelivered(ctx context.Context, id string) error
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
func (s *InMemoryStore) Finalize(_ context.Context, id string, st Status, summary, result string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status.Terminal() {
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

// MarkInputRequired implements Store.
func (s *InMemoryStore) MarkInputRequired(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok && t.Status == StatusWorking {
		t.Status = StatusInputRequired
		t.UpdatedAt = time.Now().UTC()
	}
	return nil
}

// ReclaimWorking implements Store.
func (s *InMemoryStore) ReclaimWorking(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status != StatusInputRequired {
		return false, nil
	}
	t.Status = StatusWorking
	t.UpdatedAt = time.Now().UTC()
	return true, nil
}

// ConsumeNotify and MarkNotifyDelivered deliberately leave UpdatedAt alone. For
// a terminal task that column is when it FINISHED — created→updated is the
// duration a UI shows — and delivery can happen minutes later.
func (s *InMemoryStore) ConsumeNotify(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok && t.NotifyState == NotifyPending {
		t.NotifyState = NotifyConsumed
	}
	return nil
}

// MarkNotifyDelivered implements Store.
func (s *InMemoryStore) MarkNotifyDelivered(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok && t.NotifyState == NotifyPending {
		t.NotifyState = NotifyDelivered
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
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

var _ Store = (*InMemoryStore)(nil)
