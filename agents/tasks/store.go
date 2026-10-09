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

// Store persists tasks on a transactional backend: every transition below is a
// compare-and-set, and each answers an unknown id with ErrNotFound, never
// won=false — see spec §2.13. InMemoryStore is built in; the sessions module
// ships SQL ones.
type Store interface {
	Create(ctx context.Context, t *Task) error
	Get(ctx context.Context, id string) (*Task, error)
	ByChildSession(ctx context.Context, sessionID string) (*Task, error)
	ListByParent(ctx context.Context, parentSessionID string) ([]Task, error)

	// Finalize records a terminal status, result and non-nil state in one
	// compare-and-set on runID; won=false means the row moved first — see spec §2.13.
	Finalize(ctx context.Context, id, runID string, st Status, summary, result string, state json.RawMessage) (won bool, err error)

	// RetryClaim reopens a failed task in one compare-and-set: working on
	// newRunID, attempt+1, summary and result cleared, only under maxAttempts
	// (<= 0 is no limit) — see spec §2.13.
	RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (won bool, err error)

	// Advance moves a working task from runID to nextRunID in one compare-and-set,
	// replacing State unless state is nil; the same id on both sides rewrites
	// State in place — see spec §2.13.
	Advance(ctx context.Context, id, runID, nextRunID string, state json.RawMessage) (won bool, err error)

	// ReleaseRetryClaim undoes a RetryClaim whose run never launched: failed
	// again, the attempt rolled back, the launch failure as summary/result, only
	// while runID is current and working — see spec §2.13.
	ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string) (won bool, err error)

	// MarkInputRequired flips working → input_required while runID is current;
	// a lost race is a silent no-op — see spec §2.13.
	MarkInputRequired(ctx context.Context, id, runID string) error
	// ReclaimWorking flips input_required → working while runID is current; false
	// means the approval is stale: discard it, do not retry — see spec §2.13.
	ReclaimWorking(ctx context.Context, id, runID string) (bool, error)

	// FailOrphans fails every still-working task and returns the rows, for the
	// restart sweep — see spec §2.13.
	FailOrphans(ctx context.Context) ([]Task, error)
	// ListNonTerminal returns a parent's unfinished tasks, for a teardown to stop.
	ListNonTerminal(ctx context.Context, parentSessionID string) ([]Task, error)

	Delete(ctx context.Context, id string) error
}

// InMemoryStore is a goroutine-safe Store for tests and single-process use;
// one lock guards every operation.
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

// Finalize implements Store.
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
	// Floored at 1: the rollback never goes below the original run.
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
		// A paused row is not an orphan — see spec §2.13.
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
