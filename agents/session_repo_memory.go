package agents

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// InMemoryRepo is a SessionRepo holding everything in memory.
//
// It exists for tests and short-lived processes, and it is in the core because
// SessionRepo is: an interface whose only implementations live in other modules
// cannot be exercised without pulling one of them in, which is a heavy way to
// test a lifecycle.
type InMemoryRepo struct {
	mu       sync.Mutex
	sessions map[string]*InMemoryStorage
	order    []string
	nextID   int
}

// NewInMemoryRepo returns an empty repo.
func NewInMemoryRepo() *InMemoryRepo {
	return &InMemoryRepo{sessions: map[string]*InMemoryStorage{}}
}

// Create implements SessionRepo.
func (r *InMemoryRepo) Create(_ context.Context, opts CreateOptions) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := opts.ID
	if id == "" {
		r.nextID++
		id = "sess-" + time.Now().UTC().Format("20060102150405") + "-" + strconv.Itoa(r.nextID)
	}
	if _, exists := r.sessions[id]; exists {
		return nil, NewUserError("session %q already exists", id)
	}
	st := NewInMemoryStorage(id)
	st.SetTitle(opts.Title)
	st.SetHidden(opts.Hidden)
	r.sessions[id] = st
	r.order = append(r.order, id)
	return NewSession(st), nil
}

// Open implements SessionRepo. An unknown id is an error, never an empty
// session: a wrong id that reads as a fresh conversation makes a run start over
// instead of continuing, which is worse than failing.
func (r *InMemoryRepo) Open(_ context.Context, id string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return NewSession(st), nil
}

// List implements SessionRepo.
func (r *InMemoryRepo) List(ctx context.Context, opts ListOptions) ([]SessionMetadata, error) {
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	stores := make([]*InMemoryStorage, 0, len(ids))
	for _, id := range ids {
		stores = append(stores, r.sessions[id])
	}
	r.mu.Unlock()

	out := make([]SessionMetadata, 0, len(stores))
	for _, st := range stores {
		md, err := st.Metadata(ctx)
		if err != nil {
			return nil, err
		}
		if md.Hidden && !opts.IncludeHidden {
			continue
		}
		out = append(out, md)
	}
	return out, nil
}

// Delete implements SessionRepo. Deleting an unknown session is not an error:
// the caller wanted it gone, and it is.
//
// A handle already handed out is retired with it: a write through one must
// refuse rather than land in storage nothing references (spec §2.5e2). The
// other three repos prove their destination on every write; this one has no
// row or file to check, so the storage itself is marked.
func (r *InMemoryRepo) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.sessions[id]; st != nil {
		st.retire()
	}
	delete(r.sessions, id)
	for i, v := range r.order {
		if v == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return nil
}

var _ SessionRepo = (*InMemoryRepo)(nil)
