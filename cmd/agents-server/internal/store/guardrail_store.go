package store

import "github.com/uptrace/bun"

// GuardrailStore manages guardrail definitions.
type GuardrailStore struct {
	*CrudStore[Guardrail]
}

// NewGuardrailStore returns a GuardrailStore backed by db.
func NewGuardrailStore(db *bun.DB) *GuardrailStore {
	return &GuardrailStore{NewCrudStore[Guardrail](db, "guardrail", "updated_at DESC")}
}
