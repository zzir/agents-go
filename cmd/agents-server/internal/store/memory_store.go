package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/zzir/agents-go/agents/session"
)

// Memory write refusals, each one policy rule.
var (
	ErrMemoryScope     = errors.New("no such memory scope")
	ErrMemoryTooLarge  = errors.New("memory exceeds the scope's size limit")
	ErrMemoryLimit     = errors.New("the scope holds its limit of memories")
	ErrMemoryForbidden = errors.New("the scope is not yours to write")
)

// MemoryScope addresses one scope's rows: the kind, its id, and for a
// session its generation.
type MemoryScope struct {
	Kind string
	ID   string
	Gen  string
}

// SessionMemoryScope is the scope of a session's own memory.
func SessionMemoryScope(ref session.Ref) MemoryScope {
	return MemoryScope{Kind: MemoryScopeSession, ID: ref.ID, Gen: ref.Gen}
}

// MemoryStore persists memories.
type MemoryStore struct {
	*CrudStore[Memory]
	db *bun.DB
}

// NewMemoryStore returns a MemoryStore backed by db.
func NewMemoryStore(db *bun.DB) *MemoryStore {
	return &MemoryStore{CrudStore: NewCrudStore[Memory](db, "memory", "updated_at DESC"), db: db}
}

// ListInjectable returns what an agent's instructions carry: every scope the
// policy injects, an allow-list, never "all but session".
func (s *MemoryStore) ListInjectable(ctx context.Context, agentConfigID string) ([]Memory, error) {
	var memories []Memory
	q := s.db.NewSelect().Model(&memories)
	if agentConfigID != "" {
		q = q.Where("(mem.scope_kind = ? OR (mem.scope_kind = ? AND mem.scope_id = ?))", MemoryScopeGlobal, MemoryScopeAgent, agentConfigID)
	} else {
		q = q.Where("mem.scope_kind = ?", MemoryScopeGlobal)
	}
	if err := q.OrderExpr("mem.updated_at DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing memories for agent: %w", err)
	}
	return memories, nil
}

// ListConfig lists the configuration memories the caller may see: global
// rows, and agent rows of agents visible to the caller; kind and scopeID
// narrow further. Session memory is read under its session, never here.
func (s *MemoryStore) ListConfig(ctx context.Context, callerID string, admin bool, kind, scopeID string) ([]Memory, error) {
	var memories []Memory
	q := s.db.NewSelect().Model(&memories).
		Where("mem.scope_kind IN (?, ?)", MemoryScopeGlobal, MemoryScopeAgent)
	if kind != "" {
		q = q.Where("mem.scope_kind = ?", kind)
	}
	if scopeID != "" {
		q = q.Where("mem.scope_id = ?", scopeID)
	}
	if !admin {
		// scope_id is text and the agent id a uuid: PostgreSQL compares the
		// two only through a cast, which SQLite accepts as well.
		visible := s.db.NewSelect().Model((*AgentConfig)(nil)).ColumnExpr("CAST(id AS TEXT)").
			Where("scope = ? OR owner_id = ?", ScopeGlobal, callerID)
		q = q.Where("(mem.scope_kind = ? OR mem.scope_id IN (?))", MemoryScopeGlobal, visible)
	}
	if err := q.OrderExpr("mem.updated_at DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing memories: %w", err)
	}
	return memories, nil
}

// ListScope lists one scope's memories by key.
func (s *MemoryStore) ListScope(ctx context.Context, sc MemoryScope) ([]Memory, error) {
	var memories []Memory
	if err := scopedMemories(s.db.NewSelect().Model(&memories), sc).
		OrderExpr("mem.key ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing %s memories: %w", sc.Kind, err)
	}
	return memories, nil
}

// GetByKey reads one memory; ErrNotFound when the scope has no such key.
func (s *MemoryStore) GetByKey(ctx context.Context, sc MemoryScope, key string) (*Memory, error) {
	m := new(Memory)
	if err := scopedMemories(s.db.NewSelect().Model(m), sc).Where("mem.key = ?", key).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading memory %q: %w", key, err)
	}
	return m, nil
}

// Upsert writes m under its scope and key, creating or replacing, within the
// scope's policy. guard runs first, in the same transaction, for the write
// rules only the caller knows (an agent's edit rule); nil is unguarded.
func (s *MemoryStore) Upsert(ctx context.Context, m *Memory, guard func(ctx context.Context, tx bun.Tx) error) error {
	return s.write(ctx, m, false, guard)
}

// AppendContent adds text to the end of a memory, creating it when new; the
// writer and owner are stamped on the row either way.
func (s *MemoryStore) AppendContent(ctx context.Context, sc MemoryScope, key, text, writtenBy, ownerID string, guard func(ctx context.Context, tx bun.Tx) error) error {
	return s.write(ctx, &Memory{
		ScopeKind: sc.Kind, ScopeID: sc.ID, Gen: sc.Gen, Key: key,
		Content: text, WrittenBy: writtenBy, OwnerID: ownerID,
	}, true, guard)
}

// write is the one transaction behind Upsert and AppendContent. Two writers
// racing to CREATE the same key both pass the locked read; the unique index
// stops the second, which is retried once against the row the first made.
func (s *MemoryStore) write(ctx context.Context, m *Memory, appendTo bool, guard func(ctx context.Context, tx bun.Tx) error) error {
	var err error
	for range 2 {
		err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return upsertMemory(ctx, tx, m, appendTo, guard)
		})
		if _, dup := UniqueViolation(err); err == nil || !dup {
			return err
		}
	}
	return err
}

func upsertMemory(ctx context.Context, tx bun.Tx, m *Memory, appendTo bool, guard func(ctx context.Context, tx bun.Tx) error) error {
	policy, ok := MemoryPolicyFor(m.ScopeKind)
	if !ok {
		return fmt.Errorf("%w: %q", ErrMemoryScope, m.ScopeKind)
	}
	if guard != nil {
		if err := guard(ctx, tx); err != nil {
			return err
		}
	}
	// The row is read under a lock, so two appends never both start from
	// the same content and one overwrite the other's.
	sc := MemoryScope{Kind: m.ScopeKind, ID: m.ScopeID, Gen: m.Gen}
	prev := new(Memory)
	q := scopedMemories(tx.NewSelect().Model(prev), sc).Where("mem.key = ?", m.Key)
	if tx.Dialect().Name() == dialect.PG {
		q = q.For("UPDATE")
	}
	err := q.Scan(ctx)
	if err == nil && appendTo {
		m.Content = prev.Content + m.Content
	}
	if len(m.Content) > policy.MaxBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrMemoryTooLarge, len(m.Content), policy.MaxBytes)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Two creates of different keys never meet on a row lock, so the
		// count below is serialized per scope on PostgreSQL by an advisory
		// lock; SQLite's single writer serializes by itself.
		if tx.Dialect().Name() == dialect.PG {
			if _, lerr := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext(?))", sc.Kind+"|"+sc.ID+"|"+sc.Gen); lerr != nil {
				return fmt.Errorf("locking %s memories: %w", sc.Kind, lerr)
			}
		}
		n, cerr := scopedMemories(tx.NewSelect().Model((*Memory)(nil)), sc).Count(ctx)
		if cerr != nil {
			return fmt.Errorf("counting %s memories: %w", sc.Kind, cerr)
		}
		if n >= policy.MaxKeys {
			return fmt.Errorf("%w: %d", ErrMemoryLimit, policy.MaxKeys)
		}
		if _, err := tx.NewInsert().Model(m).Exec(ctx); err != nil {
			return fmt.Errorf("writing memory %q: %w", m.Key, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading memory %q: %w", m.Key, err)
	}
	m.ID, m.CreatedAt = prev.ID, prev.CreatedAt
	m.UpdatedAt = time.Now().UTC()
	if _, err := tx.NewUpdate().Model(m).
		Column("content", "metadata", "written_by", "owner_id", "updated_at").
		Where("id = ?", prev.ID).Exec(ctx); err != nil {
		return fmt.Errorf("replacing memory %q: %w", m.Key, err)
	}
	return nil
}

// scopedMemories narrows a query to one scope's rows.
func scopedMemories(q *bun.SelectQuery, sc MemoryScope) *bun.SelectQuery {
	return q.Where("mem.scope_kind = ?", sc.Kind).Where("mem.scope_id = ?", sc.ID).Where("mem.gen = ?", sc.Gen)
}

// deleteMemoriesOf removes every memory of one scope id, whatever its
// generation: a session's or an agent's cascade.
func deleteMemoriesOf(ctx context.Context, db bun.IDB, kind, id string) error {
	if _, err := db.NewDelete().Model((*Memory)(nil)).
		Where("scope_kind = ?", kind).Where("scope_id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("deleting %s memories of %s: %w", kind, id, err)
	}
	return nil
}

// copySessionMemories gives a fork its own copy of the source's session memory.
func copySessionMemories(ctx context.Context, tx bun.IDB, src, dst session.Ref) error {
	var rows []Memory
	if err := scopedMemories(tx.NewSelect().Model(&rows), SessionMemoryScope(src)).Scan(ctx); err != nil {
		return fmt.Errorf("fork memories read: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		rows[i].ID = ""
		rows[i].ScopeID, rows[i].Gen = dst.ID, dst.Gen
	}
	if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("fork memories write: %w", err)
	}
	return nil
}
