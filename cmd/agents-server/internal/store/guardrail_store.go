package store

import (
	"context"

	"github.com/uptrace/bun"
)

// GuardrailStore manages guardrail definitions.
type GuardrailStore struct {
	*CrudStore[Guardrail]
}

// NewGuardrailStore returns a GuardrailStore backed by db. Names are unique
// (idx_guardrails_name); a duplicate is a UNIQUE error.
func NewGuardrailStore(db *bun.DB) *GuardrailStore {
	return &GuardrailStore{NewCrudStore[Guardrail](db, "guardrail", "created_at DESC")}
}

// Create inserts the guardrail.
func (s *GuardrailStore) Create(ctx context.Context, g *Guardrail) error { return s.create(ctx, g) }

// Update overwrites the guardrail; ErrNotFound when absent.
func (s *GuardrailStore) Update(ctx context.Context, id string, g *Guardrail) error {
	return s.update(ctx, id, g)
}

// Delete removes the guardrail; ErrNotFound when absent.
func (s *GuardrailStore) Delete(ctx context.Context, id string) error { return s.delete(ctx, id) }
