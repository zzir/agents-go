package session

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// InMemoryRepo is a Repo holding everything in memory, for tests and
// short-lived processes.
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

// Create implements Repo.
func (r *InMemoryRepo) Create(_ context.Context, opts CreateOptions) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := opts.ID
	if id == "" {
		r.nextID++
		id = "sess-" + time.Now().UTC().Format("20060102150405") + "-" + strconv.Itoa(r.nextID)
	}
	if _, exists := r.sessions[id]; exists {
		return nil, fmt.Errorf("session: session %q already exists", id)
	}
	st := NewInMemoryStorage(id)
	st.SetTitle(opts.Title)
	st.SetHidden(opts.Hidden)
	r.sessions[id] = st
	r.order = append(r.order, id)
	return NewSession(st), nil
}

// Open implements Repo. An unknown id is ErrNotFound, never an empty session
// (spec §2.5e).
func (r *InMemoryRepo) Open(_ context.Context, id string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return NewSession(st), nil
}

// List implements Repo: newest first, cut to ListOptions.Limit — the same
// answer every repo gives (spec §2.5e2).
func (r *InMemoryRepo) List(ctx context.Context, opts ListOptions) ([]Metadata, error) {
	r.mu.Lock()
	stores := make([]*InMemoryStorage, 0, len(r.order))
	for _, id := range r.order {
		stores = append(stores, r.sessions[id])
	}
	r.mu.Unlock()

	out := make([]Metadata, 0, len(stores))
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
	// Stable, so sessions sharing an UpdatedAt keep creation order instead of
	// shuffling between two calls that read the same sessions.
	slices.SortStableFunc(out, func(a, b Metadata) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	if opts.Limit > 0 && opts.Limit < len(out) {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Delete implements Repo. An unknown id is not an error, and a handle already
// handed out is retired so a later write refuses — spec §2.5e2.
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

var _ Repo = (*InMemoryRepo)(nil)
