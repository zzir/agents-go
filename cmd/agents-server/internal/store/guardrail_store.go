package store

import "github.com/uptrace/bun"

// GuardrailStore manages guardrail definitions.
type GuardrailStore struct {
	*CrudStore[Guardrail]
}

// NewGuardrailStore returns a GuardrailStore backed by db. (type, name)
// uniqueness is enforced by the DB (idx_guardrails_type_name); a duplicate
// surfaces as a UNIQUE-constraint error that handlers map to 409.
func NewGuardrailStore(db *bun.DB) *GuardrailStore {
	return &GuardrailStore{NewCrudStore[Guardrail](db, "guardrail", "created_at DESC")}
}
