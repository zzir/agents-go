package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/memory"
)

// RunMemory implements memory.Store for a run: session memory under the
// session's generation, agent memory under the agent's edit rule, every
// write stamped as the model's on the session owner's behalf.
type RunMemory struct {
	db       *bun.DB
	memories *MemoryStore
	ownerID  string
	admin    bool
}

// NewRunMemory binds a run's memory access to its session owner.
func NewRunMemory(db *bun.DB, memories *MemoryStore, ownerID string, admin bool) *RunMemory {
	return &RunMemory{db: db, memories: memories, ownerID: ownerID, admin: admin}
}

var _ memory.Store = (*RunMemory)(nil)

// scope maps the SDK scope to rows: a session's current generation, or an agent.
func (rm *RunMemory) scope(ctx context.Context, sc memory.Scope) (MemoryScope, error) {
	switch sc.Kind {
	case MemoryScopeSession:
		ref, err := RefFor(ctx, rm.db, sc.ID)
		if err != nil {
			return MemoryScope{}, fmt.Errorf("memory: resolving session %s: %w", sc.ID, err)
		}
		return SessionMemoryScope(ref), nil
	case MemoryScopeAgent:
		return MemoryScope{Kind: MemoryScopeAgent, ID: sc.ID}, nil
	}
	return MemoryScope{}, fmt.Errorf("%w: %q", ErrMemoryScope, sc.Kind)
}

// guard is the write rule re-checked inside the write's transaction: an
// agent's memory is its editor's (decisions §5.29), a session's is open to
// the run that owns it.
func (rm *RunMemory) guard(sc MemoryScope) func(ctx context.Context, tx bun.Tx) error {
	if sc.Kind != MemoryScopeAgent {
		return nil
	}
	return func(ctx context.Context, tx bun.Tx) error {
		var ac AgentConfig
		if err := lockRow(ctx, tx, &ac, "id = ?", sc.ID); err != nil {
			return fmt.Errorf("memory: agent %s: %w", sc.ID, err)
		}
		if ac.OwnerID == rm.ownerID || (ac.Scope == ScopeGlobal && rm.admin) {
			return nil
		}
		return ErrMemoryForbidden
	}
}

// List implements memory.Store.
func (rm *RunMemory) List(ctx context.Context, sc memory.Scope) ([]memory.Info, error) {
	ms, err := rm.scope(ctx, sc)
	if err != nil {
		return nil, err
	}
	rows, err := rm.memories.ListScope(ctx, ms)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Info, len(rows))
	for i, r := range rows {
		out[i] = memory.Info{Key: r.Key, Bytes: len(r.Content), UpdatedAt: r.UpdatedAt}
	}
	return out, nil
}

// Read implements memory.Store.
func (rm *RunMemory) Read(ctx context.Context, sc memory.Scope, key string) (string, error) {
	ms, err := rm.scope(ctx, sc)
	if err != nil {
		return "", err
	}
	m, err := rm.memories.GetByKey(ctx, ms, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", memory.ErrNotFound
		}
		return "", err
	}
	return m.Content, nil
}

// Write implements memory.Store.
func (rm *RunMemory) Write(ctx context.Context, sc memory.Scope, key, text string) error {
	ms, err := rm.scope(ctx, sc)
	if err != nil {
		return err
	}
	return rm.memories.Upsert(ctx, &Memory{
		ScopeKind: ms.Kind, ScopeID: ms.ID, Gen: ms.Gen, Key: key, Content: text,
		WrittenBy: MemoryWrittenByModel, OwnerID: rm.ownerID,
	}, rm.guard(ms))
}

// Append implements memory.Store.
func (rm *RunMemory) Append(ctx context.Context, sc memory.Scope, key, text string) error {
	ms, err := rm.scope(ctx, sc)
	if err != nil {
		return err
	}
	return rm.memories.AppendContent(ctx, ms, key, text, MemoryWrittenByModel, rm.ownerID, rm.guard(ms))
}

// memoryReader is one scope's rows as a read-only memory.Store, for the
// snapshot a reset carries when only the rows' scope is known.
type memoryReader struct {
	store *MemoryStore
	scope MemoryScope
}

func (r *memoryReader) List(ctx context.Context, _ memory.Scope) ([]memory.Info, error) {
	rows, err := r.store.ListScope(ctx, r.scope)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Info, len(rows))
	for i, row := range rows {
		out[i] = memory.Info{Key: row.Key, Bytes: len(row.Content), UpdatedAt: row.UpdatedAt}
	}
	return out, nil
}

func (r *memoryReader) Read(ctx context.Context, _ memory.Scope, key string) (string, error) {
	m, err := r.store.GetByKey(ctx, r.scope, key)
	if err != nil {
		return "", err
	}
	return m.Content, nil
}

func (r *memoryReader) Write(context.Context, memory.Scope, string, string) error {
	return errors.New("memory: read-only")
}

func (r *memoryReader) Append(context.Context, memory.Scope, string, string) error {
	return errors.New("memory: read-only")
}
