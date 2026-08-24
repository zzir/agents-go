// Package store is the SQLite-backed persistence layer (bun ORM) for sessions, messages, agent configs, MCP servers, memories, settings, provider routes, sandboxes, and trace events.
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

// NewAgentConfigStore returns an AgentConfigStore backed by db. Agent-name
// uniqueness is enforced by the DB (idx_agent_configs_name); a duplicate
// surfaces as a UNIQUE-constraint error that handlers map to 409.
func NewAgentConfigStore(db *bun.DB) *AgentConfigStore {
	return &AgentConfigStore{CrudStore: NewCrudStore[AgentConfig](db, "agent config", "created_at DESC").withSecrets(sealAgentConfig, openAgentConfig), db: db}
}

// Create writes the agent only if its provider still exists, atomically — the
// same check-then-write guard the route store has, so a provider deleted
// between the handler's validation and this write cannot leave a dangling
// provider_id (ErrProviderRef if it does; an empty provider_id is the default).
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
