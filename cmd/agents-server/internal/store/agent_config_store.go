// Package store is the persistence layer (bun ORM; SQLite or PostgreSQL) for
// sessions, configuration entities, tasks, traces and audit events.
package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// AgentConfigStore persists agent configurations.
type AgentConfigStore struct {
	*CrudStore[AgentConfig]
	db *bun.DB
}

// NewAgentConfigStore returns an AgentConfigStore backed by db. Names are
// unique per scope (partial indexes, spec §5.29); a duplicate surfaces as a
// UNIQUE-constraint error that handlers map to 409.
func NewAgentConfigStore(db *bun.DB) *AgentConfigStore {
	return &AgentConfigStore{CrudStore: NewCrudStore[AgentConfig](db, "agent config", "created_at DESC").withSecrets(sealAgentConfig, openAgentConfig), db: db}
}

// Create writes the agent in the provider-guarded transaction: a provider
// deleted or re-scoped between the handler's validation and this write is
// refused (ErrProviderRef / ErrProviderScope; empty provider_id skips it).
func (s *AgentConfigStore) Create(ctx context.Context, ac *AgentConfig) error {
	return sealedWrite(ac, sealAgentConfig, openAgentConfig, func() error {
		return writeReferencingProvider(ctx, s.db, ac.ProviderID, func(ctx context.Context, tx bun.Tx, pv *Provider) error {
			if err := refProviderScope(pv, ac.Scope, ac.OwnerID); err != nil {
				return err
			}
			_, err := tx.NewInsert().Model(ac).Exec(ctx)
			return err
		})
	})
}

// Update overwrites the agent config, under the same provider guard as Create
// — re-pointing an agent at a provider races a delete exactly like a create
// does. The stored row is read in the same transaction and handed to prepare
// (nil to skip), so a masked fallback-model key keeps its stored value.
// Returns an ErrNotFound-wrapping error when the row doesn't exist.
func (s *AgentConfigStore) Update(ctx context.Context, id string, m *AgentConfig, prepare func(prev *AgentConfig) error) error {
	err := writeReferencingProvider(ctx, s.db, m.ProviderID, func(ctx context.Context, tx bun.Tx, pv *Provider) error {
		return s.updateFrom(ctx, tx, id, m, func(prev *AgentConfig) error {
			if prepare != nil {
				if err := prepare(prev); err != nil {
					return err
				}
			}
			// prepare restored m's scope/owner from prev; check with the
			// values the row will actually hold.
			return refProviderScope(pv, m.Scope, m.OwnerID)
		})
	})
	if err != nil {
		return fmt.Errorf("updating agent config %s: %w", id, err)
	}
	return nil
}
