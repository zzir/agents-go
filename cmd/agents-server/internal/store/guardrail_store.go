package store

import "github.com/uptrace/bun"

// GuardrailStore manages guardrail definitions.
type GuardrailStore struct {
	*CrudStore[Guardrail]
}

// NewGuardrailStore returns a GuardrailStore backed by db. (type, name) is
// unique (idx_guardrails_type_name); a duplicate is a UNIQUE error.
func NewGuardrailStore(db *bun.DB) *GuardrailStore {
	return &GuardrailStore{NewCrudStore[Guardrail](db, "guardrail", "created_at DESC")}
}
