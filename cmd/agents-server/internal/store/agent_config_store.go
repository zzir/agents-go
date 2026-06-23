// Package store is the SQLite-backed persistence layer (bun ORM) for sessions, messages, agent configs, MCP servers, memories, settings, provider routes, sandboxes, and trace events.
package store

import "github.com/uptrace/bun"

// AgentConfigStore persists agent configurations.
type AgentConfigStore struct {
	*CrudStore[AgentConfig]
}

// NewAgentConfigStore returns an AgentConfigStore backed by db.
func NewAgentConfigStore(db *bun.DB) *AgentConfigStore {
	return &AgentConfigStore{NewCrudStore[AgentConfig](db, "agent config", "updated_at DESC")}
}
