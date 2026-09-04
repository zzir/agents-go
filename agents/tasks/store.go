package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"
)

// ErrNotFound is returned by Store lookups for an unknown task.
var ErrNotFound = errors.New("tasks: not found")

// Store persists tasks. It requires TRANSACTIONAL semantics: every transition
// below is a compare-and-set, and correctness depends on it — spec §2.13. The
// SDK ships InMemoryStore; the sessions module ships a SQL one.
type Store interface {
	Create(ctx context.Context, t *Task) error
	Get(ctx context.Context, id string) (*Task, error)
	ByChildSession(ctx context.Context, sessionID string) (*Task, error)
	ListByParent(ctx context.Context, parentSessionID string) ([]Task, error)

	// Finalize records a terminal status and its result in ONE conditional
	// transition, only while the task is non-terminal AND still on the attempt
	// runID names. won=false means another finalizer owned the transition or the
	// attempt is no longer current: do nothing further. state, when non-nil, is
	// the job's final State, written in the same transition — spec §2.13.
	Finalize(ctx context.Context, id, runID string, st Status, summary, result string, state json.RawMessage) (won bool, err error)

	// RetryClaim reopens a failed task for another attempt, in one transition
	// and only while it is failed and under maxAttempts (counts the original
	// run; <= 0 is no limit): working, run_id=newRunID, attempt+1, summary and
	// result cleared. won=false means the row could not be claimed (not failed,
	// out of attempts, a lost race); an unknown id is ErrNotFound — spec §2.13.
	RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (won bool, err error)

	// Advance moves a working task on to its next run in one transition, only
	// while runID is current: run_id=nextRunID, State replaced by state (nil
	// keeps it). Attempt is untouched; nextRunID may equal runID, which
	// rewrites State in place. won=false means another writer moved the task
	// first; an unknown id is ErrNotFound — spec §2.13.
	Advance(ctx context.Context, id, runID, nextRunID string, state json.RawMessage) (won bool, err error)

	// ReleaseRetryClaim undoes a RetryClaim whose run never launched: failed
	// again, the attempt rolled back (it counts runs the task HAD), the launch
	// failure recorded as summary/result — an ending reported like any other.
	// Only while runID is current and the row is working; won=false means
	// another writer moved the task first — spec §2.13.
	ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string) (won bool, err error)

	// MarkInputRequired flips working → input_required, only while runID is the
	// current attempt. Best-effort: a concurrent terminal transition or a newer
	// attempt wins (spec §2.13).
	MarkInputRequired(ctx context.Context, id, runID string) error
	// ReclaimWorking flips input_required → working when an approval resumes
	// the run, only while runID is current. false means the resume must be
	// abandoned and a stale approval discarded, not retried.
	ReclaimWorking(ctx context.Context, id, runID string) (bool, error)

	// FailOrphans marks every still-working task failed and returns them, so
	// each parent can be told (spec §2.13). Called at startup.
	FailOrphans(ctx context.Context) ([]Task, error)
	// ListNonTerminal returns a parent's unfinished tasks, for a teardown that
	// must stop them.
	ListNonTerminal(ctx context.Context, parentSessionID string) ([]Task, error)

	Delete(ctx context.Context, id string) error
}

// InMemoryStore is a goroutine-safe Store for tests and single-process use.
// One lock guards every operation: the operations are short and a torn read of
// the state machine is a wrong answer, not a slow one.
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
	for _, v := range slices.Backward(s.order) {
		if t := s.tasks[v]; t != nil && t.ParentSessionID == parentSessionID {
			out = append(out, *t)
		}
	}
	return out, nil
}

// Finalize implements Store. The whole transition happens under one lock, so a
// reader can never see a terminal task whose result has not landed.
func (s *InMemoryStore) Finalize(_ context.Context, id, runID string, st Status, summary, result string, state json.RawMessage) (bool, error) {
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
	if summary != "" {
		t.Summary = summary
	}
	if result != "" {
		t.Result = result
	}
	if state != nil {
		t.State = append(json.RawMessage(nil), state...)
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
	// The previous attempt's account, cleared: it describes a run that is no
	// longer this task's.
	t.Summary, t.Result = "", ""
	t.UpdatedAt = time.Now().UTC()
	return true, nil
}

// Advance implements Store.
func (s *InMemoryStore) Advance(_ context.Context, id, runID, nextRunID string, state json.RawMessage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, ErrNotFound
	}
	if t.Status != StatusWorking || t.RunID != runID {
		return false, nil
	}
	t.RunID = nextRunID
	if state != nil {
		t.State = slices.Clone(state)
	}
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

// FailOrphans implements Store.
func (s *InMemoryStore) FailOrphans(_ context.Context) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, id := range s.order {
		t := s.tasks[id]
		// input_required rows are kept: their pending approval persists and
		// resumes the run, so they are not orphans.
		if t == nil || t.Status != StatusWorking {
			continue
		}
		t.Status = StatusFailed
		t.Summary = "the process restarted while the task was running"
		t.UpdatedAt = time.Now().UTC()
		out = append(out, *t)
	}
	return out, nil
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
