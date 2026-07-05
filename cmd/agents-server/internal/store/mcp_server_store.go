package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// McpServerStore persists MCP server connection configs.
type McpServerStore struct {
	*CrudStore[McpServerConfig]
}

// NewMcpServerStore returns an McpServerStore backed by db.
func NewMcpServerStore(db *bun.DB) *McpServerStore {
	return &McpServerStore{NewCrudStore[McpServerConfig](db, "mcp server config", "updated_at DESC")}
}

// Update overwrites the server config but preserves the oauth_token column.
// Returns an ErrNotFound-wrapping error when the row doesn't exist.
func (s *McpServerStore) Update(ctx context.Context, id string, m *McpServerConfig) error {
	res, err := s.db.NewUpdate().Model(m).
		ExcludeColumn("id", "created_at", "oauth_token").
		Where("id = ?", id).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("updating mcp server config %s: %w", id, err)
	}
	return nil
}

// SaveOAuthToken persists the serialized OAuth token for the given server,
// updating only the oauth_token column so it is not cleared by regular CRUD
// updates.
func (s *McpServerStore) SaveOAuthToken(ctx context.Context, id, tokenJSON string) error {
	_, err := s.db.NewUpdate().
		Model((*McpServerConfig)(nil)).
		Set("oauth_token = ?", tokenJSON).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("saving oauth token for %s: %w", id, err)
	}
	return nil
}

// ClearOAuthToken removes the persisted OAuth token for the given server.
func (s *McpServerStore) ClearOAuthToken(ctx context.Context, id string) error {
	return s.SaveOAuthToken(ctx, id, "")
}
