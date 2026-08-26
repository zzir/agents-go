// Package store is the persistence layer (bun ORM; SQLite or PostgreSQL) for
// sessions, configuration entities, tasks, traces and audit events.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// AgentConfigStore persists agent configurations.
type AgentConfigStore struct {
	*CrudStore[AgentConfig]
	db *bun.DB
}

// NewAgentConfigStore returns an AgentConfigStore backed by db. Names are
// unique per scope (partial indexes, decisions §5.29); a duplicate surfaces as a
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

// TransferOwner hands the agent to newOwner, re-checking the leg that spends
// a credential AS the new owner: an agent pointed at somebody's private
// provider would answer 204 here and then fail every run (decisions §5.29). The
// lock, the check and the write share one transaction, so a provider flip
// cannot slip between them. The advisory legs (MCP servers, skills, handoff
// targets) are the handler's to validate, as they are on a save.
func (s *AgentConfigStore) TransferOwner(ctx context.Context, id, newOwner string) error {
	err := s.rewriteScopeOrOwner(ctx, id, func(ac *AgentConfig) (string, string) {
		return ac.Scope, newOwner
	}, func(ctx context.Context, tx bun.Tx) error {
		exists, err := tx.NewSelect().Model((*User)(nil)).Where("id = ?", newOwner).Exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNoSuchUser
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("transferring agent config %s: %w", id, err)
	}
	return nil
}

// SetScope flips the agent's scope with the credential leg re-checked as the
// TARGET scope, in the same transaction: a promote validated while the
// provider was global must not land after a demote made it private, which
// would leave a global agent on somebody's private key (decisions §5.29).
func (s *AgentConfigStore) SetScope(ctx context.Context, id, scope string) error {
	err := s.rewriteScopeOrOwner(ctx, id, func(ac *AgentConfig) (string, string) {
		if ac.Scope == scope {
			return "", "" // caller's same-scope check, settled here too
		}
		return scope, ac.OwnerID
	}, nil)
	if err != nil {
		return fmt.Errorf("setting agent config %s scope: %w", id, err)
	}
	return nil
}

// rewriteScopeOrOwner is the shared shape of the two writes that move an
// agent between visibility contexts: lock the PROVIDER first and the agent
// second — the order every agent write uses, so two of them cannot deadlock
// — re-check the reference rule with the pair the row is about to hold, then
// write it. want returns the (scope, owner) to land; an empty scope means the
// row already holds it (ErrSameScope). pre runs first, for checks that need
// no row.
func (s *AgentConfigStore) rewriteScopeOrOwner(ctx context.Context, id string, want func(*AgentConfig) (string, string), pre func(context.Context, bun.Tx) error) error {
	// The provider id is read unlocked only to know WHICH row to lock first;
	// the locked agent below is re-checked against it.
	probe, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return writeReferencingProvider(ctx, s.db, probe.ProviderID, func(ctx context.Context, tx bun.Tx, pv *Provider) error {
		if pre != nil {
			if err := pre(ctx, tx); err != nil {
				return err
			}
		}
		ac := new(AgentConfig)
		if err := lockRow(ctx, tx, ac, "id = ?", id); err != nil {
			return err
		}
		if ac.ProviderID != probe.ProviderID {
			// Re-pointed between the probe and the lock: the provider we hold
			// is the wrong one, and taking the other now would invert the lock
			// order. The caller retries against the current row.
			return ErrOwnershipChanged
		}
		scope, owner := want(ac)
		if scope == "" {
			return ErrSameScope
		}
		if err := refProviderScope(pv, scope, owner); err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*AgentConfig)(nil)).
			Set("scope = ?", scope).
			Set("owner_id = ?", owner).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		return err
	})
}
