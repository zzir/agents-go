package store

import "github.com/uptrace/bun"

// SandboxStore persists sandbox configs.
type SandboxStore struct {
	*CrudStore[SandboxConfig]
}

// NewSandboxStore returns a SandboxStore backed by db.
func NewSandboxStore(db *bun.DB) *SandboxStore {
	return &SandboxStore{NewCrudStore[SandboxConfig](db, "sandbox config", "created_at DESC")}
}
