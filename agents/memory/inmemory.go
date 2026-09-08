package memory

import (
	"context"
	"sync"
	"time"
)

// InMemoryStore is a goroutine-safe Store for tests and short-lived programs.
type InMemoryStore struct {
	mu   sync.Mutex
	data map[Scope]map[string]entry
}

type entry struct {
	text    string
	updated time.Time
}

// NewInMemoryStore returns an empty store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{data: make(map[Scope]map[string]entry)}
}

// List implements Store.
func (s *InMemoryStore) List(_ context.Context, scope Scope) ([]Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Info
	for key, e := range s.data[scope] {
		out = append(out, Info{Key: key, Bytes: len(e.text), UpdatedAt: e.updated})
	}
	sortInfos(out)
	return out, nil
}

// Read implements Store.
func (s *InMemoryStore) Read(_ context.Context, scope Scope, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[scope][key]
	if !ok {
		return "", ErrNotFound
	}
	return e.text, nil
}

// Write implements Store.
func (s *InMemoryStore) Write(_ context.Context, scope Scope, key, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(scope, key, text)
	return nil
}

// Append implements Store.
func (s *InMemoryStore) Append(_ context.Context, scope Scope, key, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(scope, key, s.data[scope][key].text+text)
	return nil
}

func (s *InMemoryStore) put(scope Scope, key, text string) {
	if s.data[scope] == nil {
		s.data[scope] = make(map[string]entry)
	}
	s.data[scope][key] = entry{text: text, updated: time.Now().UTC()}
}
