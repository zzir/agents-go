package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

// AuthModeChatGPTLogin is the one provider auth mode beyond a plain API key.
// It lives here because it is a column's vocabulary; the bridge aliases it.
const AuthModeChatGPTLogin = "chatgpt_login"

// ErrProviderRef marks a write refused because the provider it references is
// gone. The CALLER's input is what is wrong — handlers map it to 400, never
// the 404 a missing target resource gets.
var ErrProviderRef = errors.New("provider_id names no provider")

// writeReferencingProvider runs write in ONE transaction that first verifies
// providerID still exists (and passes check, when given) — closing the
// check-then-write window where a provider is deleted or rewritten between a
// handler's validation and the row landing. Inserts and updates alike: a
// re-point races the same delete. An empty providerID is the built-in default
// and skips the check.
func writeReferencingProvider(ctx context.Context, db *bun.DB, providerID string, check func(*Provider) error, write func(context.Context, bun.Tx) error) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if providerID != "" {
			pv := new(Provider)
			if err := tx.NewSelect().Model(pv).Where("id = ?", providerID).Scan(ctx); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrProviderRef
				}
				return err
			}
			if check != nil {
				if err := check(pv); err != nil {
					return err
				}
			}
		}
		return write(ctx, tx)
	})
}

// ProviderStore persists provider endpoints and their credentials.
type ProviderStore struct {
	*CrudStore[Provider]
	db *bun.DB
}

// NewProviderStore returns a ProviderStore backed by db. Name uniqueness is
// enforced by the DB (idx_providers_name); a duplicate surfaces as a
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
func (s *ProviderStore) DeleteIfUnreferenced(ctx context.Context, id string) (refs int, err error) {
	res, err := s.db.NewDelete().Model((*Provider)(nil)).
		Where("id = ?", id).
		Where(providerUnreferenced, id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting provider %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
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
