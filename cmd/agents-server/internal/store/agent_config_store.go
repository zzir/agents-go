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
}

// NewAgentConfigStore returns an AgentConfigStore backed by db.
func NewAgentConfigStore(db *bun.DB) *AgentConfigStore {
	return &AgentConfigStore{NewCrudStore[AgentConfig](db, "agent config", "updated_at DESC")}
}

// Update overwrites the agent config but preserves the chatgpt_token column.
// Returns an ErrNotFound-wrapping error when the row doesn't exist.
func (s *AgentConfigStore) Update(ctx context.Context, id string, m *AgentConfig) error {
	res, err := s.db.NewUpdate().Model(m).
		ExcludeColumn("id", "created_at", "chatgpt_token").
		Where("id = ?", id).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("updating agent config %s: %w", id, err)
	}
	return nil
}

// SaveChatGPTToken persists the serialized ChatGPT OAuth token for the given
// agent, updating only the chatgpt_token column.
func (s *AgentConfigStore) SaveChatGPTToken(ctx context.Context, id, tokenJSON string) error {
	_, err := s.db.NewUpdate().
		Model((*AgentConfig)(nil)).
		Set("chatgpt_token = ?", tokenJSON).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("saving chatgpt token for agent %s: %w", id, err)
	}
	return nil
}

// ClearChatGPTToken removes the ChatGPT OAuth token for the given agent.
func (s *AgentConfigStore) ClearChatGPTToken(ctx context.Context, id string) error {
	return s.SaveChatGPTToken(ctx, id, "")
}
