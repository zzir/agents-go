package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// MemoryStore persists memories.
type MemoryStore struct {
	*CrudStore[Memory]
}

// NewMemoryStore returns a MemoryStore backed by db.
func NewMemoryStore(db *bun.DB) *MemoryStore {
	return &MemoryStore{NewCrudStore[Memory](db, "memory", "created_at DESC")}
}

// ListForAgent returns global memories plus those scoped to agentConfigID (or
// only global memories when agentConfigID is empty).
func (s *MemoryStore) ListForAgent(ctx context.Context, agentConfigID string) ([]Memory, error) {
	var memories []Memory
	q := s.db.NewSelect().Model(&memories)
	if agentConfigID != "" {
		q = q.Where("agent_config_id IS NULL OR agent_config_id = ?", agentConfigID)
	} else {
		q = q.Where("agent_config_id IS NULL")
	}
	if err := q.OrderExpr("updated_at DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing memories for agent: %w", err)
	}
	return memories, nil
}
