package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// AuthModeChatGPTLogin is the one provider auth mode beyond a plain API key.
// It lives here because it is a column's vocabulary; the bridge aliases it.
const AuthModeChatGPTLogin = "chatgpt_login"

// ErrProviderRef marks a write refused because the provider it references is
// gone. The CALLER's input is what is wrong — handlers map it to 400, never
// the 404 a missing target resource gets.
var ErrProviderRef = errors.New("provider_id names no provider")

// ErrProviderScope marks a write refused because the provider it references
// sits outside the holder's reach (decisions §5.29). Handlers map it to 400.
var ErrProviderScope = errors.New("provider_id names a provider outside the agent's scope")

// writeReferencingProvider runs write in ONE transaction that first reads —
// and on PostgreSQL locks — the provider row the write references, closing
// the window where the provider is deleted or re-scoped between a handler's
// validation and the row landing. write receives the locked row (nil when
// providerID is empty, the no-provider default) and refuses what it cannot
// accept.
func writeReferencingProvider(ctx context.Context, db *bun.DB, providerID string, write func(ctx context.Context, tx bun.Tx, pv *Provider) error) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var pv *Provider
		if providerID != "" {
			pv = new(Provider)
			if err := lockRow(ctx, tx, pv, "id = ?", providerID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return ErrProviderRef
				}
				return err
			}
		}
		return write(ctx, tx, pv)
	})
}

// refProviderScope is the in-transaction half of the reference rule (spec
// §5.29): the holder must be able to SEE the provider it names. A nil pv (no
// provider) always passes.
func refProviderScope(pv *Provider, holderScope, holderOwner string) error {
	if pv != nil && !RefVisible(pv.Scope, pv.OwnerID, holderScope, holderOwner) {
		return ErrProviderScope
	}
	return nil
}

// ProviderStore persists provider endpoints and their credentials.
type ProviderStore struct {
	*CrudStore[Provider]
	db *bun.DB
}

// NewProviderStore returns a ProviderStore backed by db. Names are unique
// per scope (partial indexes, decisions §5.29); a duplicate surfaces as a
// UNIQUE-constraint error that handlers map to 409.
func NewProviderStore(db *bun.DB) *ProviderStore {
	return &ProviderStore{CrudStore: NewCrudStore[Provider](db, "provider", "created_at DESC").withSecrets(sealProvider, openProvider), db: db}
}

// Update overwrites the provider in one transaction that first reads the
// stored row (locked) and hands it to prepare, nil to skip — how a masked
// api_key keeps its stored value. A chatgpt_login provider keeps its
// chatgpt_token, so a rename does not log the endpoint out; any other mode
// clears it, since the UI can no longer revoke one.
func (s *ProviderStore) Update(ctx context.Context, id string, p *Provider, prepare func(prev *Provider) error) error {
	p.ID = id
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var keep []string
		if p.AuthMode == AuthModeChatGPTLogin {
			keep = []string{"chatgpt_token"}
		} else {
			p.ChatGPTToken = ""
		}
		return s.updateFrom(ctx, tx, id, p, prepare, keep...)
	})
	if err != nil {
		return fmt.Errorf("updating provider %s: %w", id, err)
	}
	return nil
}

// SaveChatGPTToken persists the serialized OAuth token, updating only that
// column. The token belongs to the ENDPOINT, so every agent pointed at this
// provider shares the one login.
func (s *ProviderStore) SaveChatGPTToken(ctx context.Context, id, tokenJSON string) error {
	return updateColumn(ctx, s.db, (*Provider)(nil), "provider chatgpt token", id, "chatgpt_token", sealSecret(labelProviderChatGPTToken, tokenJSON))
}

// ClearChatGPTToken removes the stored OAuth token.
func (s *ProviderStore) ClearChatGPTToken(ctx context.Context, id string) error {
	return s.SaveChatGPTToken(ctx, id, "")
}

// providerUnreferenced is the clause that keeps a delete from stranding a
// reference: an agent pointing here blocks it.
const providerUnreferenced = `NOT EXISTS (SELECT 1 FROM agent_configs WHERE provider_id = ?)`

// DeleteIfUnreferenced deletes the provider only while nothing references it —
// one atomic statement, closing the race where an agent is repointed here
// between a count and the delete (which would leave that agent naming a
// provider that no longer exists). It returns how many references blocked the
// delete: 0 with a nil error means deleted. A missing provider is ErrNotFound,
// a different answer from refused, and the handler maps them to 404 vs 409.
func (s *ProviderStore) DeleteIfUnreferenced(ctx context.Context, id, expectOwner string) (refs int, err error) {
	res, err := s.db.NewDelete().Model((*Provider)(nil)).
		Where("id = ?", id).
		Where("owner_id = ?", expectOwner). // the pair the caller was authorized against
		Where(providerUnreferenced, id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting provider %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	cur, gerr := s.Get(ctx, id)
	if gerr == nil && cur.OwnerID != expectOwner {
		return 0, fmt.Errorf("deleting provider %s: %w", id, ErrOwnershipChanged)
	}
	return s.explainRefusal(ctx, id)
}

// explainRefusal tells "no such provider" apart from "still referenced" after a
// conditional delete matched nothing.
func (s *ProviderStore) explainRefusal(ctx context.Context, id string) (int, error) {
	exists, err := s.db.NewSelect().Model((*Provider)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting provider %s: %w", id, err)
	}
	if !exists {
		return 0, fmt.Errorf("deleting provider %s: %w", id, ErrNotFound)
	}
	agents, err := s.db.NewSelect().Model((*AgentConfig)(nil)).Where("provider_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting agents on provider %s: %w", id, err)
	}
	return agents, nil
}

// NormalizeProvider trims the fields whose spelling is noise and fills the
// defaults, so two rows that mean the same endpoint are stored the same way.
// It does NOT validate the type or auth mode: those are the provider
// registry's answer, and the registry lives in bridge (which imports this
// package, not the other way round) — the handler checks them on save.
func NormalizeProvider(p *Provider) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Type = strings.TrimSpace(p.Type)
	p.AuthMode = strings.TrimSpace(p.AuthMode)
	p.APIKey = strings.TrimSpace(p.APIKey)
	// A trailing slash makes an otherwise identical endpoint compare unequal,
	// which would refuse a masked key's restore for no reason.
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	return nil
}

// DemoteToPrivate flips the provider back into its author's private set,
// refusing while any agent a demote would strand — a global agent, or another
// owner's private one — still references it. Count and flip share one
// transaction with the row locked, so a racing agent write cannot pin a
// global agent to a just-privatized key (decisions §5.29). Returns the foreign
// count, non-zero meaning nothing was flipped; ErrNotFound when the row is
// gone.
func (s *ProviderStore) DemoteToPrivate(ctx context.Context, id string) (int, error) {
	var refs int
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		pv := new(Provider)
		if err := lockRow(ctx, tx, pv, "id = ?", id); err != nil {
			return err
		}
		if pv.Scope != ScopeGlobal {
			return ErrSameScope // a second racing demote must not flip twice
		}
		n, err := countStrandedRefs(ctx, tx, id, pv.OwnerID)
		if err != nil {
			return err
		}
		if n > 0 {
			refs = n
			return nil
		}
		res, err := tx.NewUpdate().Model((*Provider)(nil)).
			Set("scope = ?", ScopePrivate).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("demoting provider %s: %w", id, err)
	}
	return refs, nil
}

// countStrandedRefs counts the agents that would lose this provider were it
// private to owner: a global agent, or one another member owns (decisions §5.29's
// RefVisible, as a query).
func countStrandedRefs(ctx context.Context, tx bun.Tx, providerID, owner string) (int, error) {
	return tx.NewSelect().Model((*AgentConfig)(nil)).
		Where("provider_id = ?", providerID).
		Where("(scope = ? OR owner_id != ?)", ScopeGlobal, owner).
		Count(ctx)
}

// TransferOwner hands the provider — credential included — to newOwner. A
// PRIVATE provider carries its references with it, so the transfer is refused
// while any agent would be stranded (the same guard a demote carries: a key
// must not silently vanish from under a run — decisions §5.29). Returns the
// stranded count, non-zero meaning nothing moved.
func (s *ProviderStore) TransferOwner(ctx context.Context, id, newOwner string) (int, error) {
	var refs int
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		exists, err := tx.NewSelect().Model((*User)(nil)).Where("id = ?", newOwner).Exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNoSuchUser
		}
		pv := new(Provider)
		if err := lockRow(ctx, tx, pv, "id = ?", id); err != nil {
			return err
		}
		if pv.Scope == ScopePrivate {
			n, err := countStrandedRefs(ctx, tx, id, newOwner)
			if err != nil {
				return err
			}
			if n > 0 {
				refs = n
				return nil
			}
		}
		res, err := tx.NewUpdate().Model((*Provider)(nil)).
			Set("owner_id = ?", newOwner).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("transferring provider %s: %w", id, err)
	}
	return refs, nil
}
