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

// NewMcpServerStore returns an McpServerStore backed by db. Names are unique
// per scope (partial indexes, decisions §5.29); a duplicate is a UNIQUE error.
func NewMcpServerStore(db *bun.DB) *McpServerStore {
	return &McpServerStore{NewCrudStore[McpServerConfig](db, "mcp server config", "created_at DESC").withSecrets(sealMcpServer, openMcpServer)}
}

// Update overwrites the server config; the stored row is read in the same
// transaction and handed to prepare (nil to skip). oauth_token is copied onto
// m before prepare runs, so prepare can also clear it. ErrNotFound when absent.
func (s *McpServerStore) Update(ctx context.Context, id string, m *McpServerConfig, prepare func(prev *McpServerConfig) error) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.updateFrom(ctx, tx, id, m, func(prev *McpServerConfig) error {
			m.OAuthToken = prev.OAuthToken
			if prepare == nil {
				return nil
			}
			return prepare(prev)
		})
	})
	if err != nil {
		return fmt.Errorf("updating mcp server config %s: %w", id, err)
	}
	return nil
}

// SaveOAuthToken persists the serialized OAuth token for the given server,
// updating only the oauth_token column.
func (s *McpServerStore) SaveOAuthToken(ctx context.Context, id, tokenJSON string) error {
	// updateColumn enforces the row exists, so a token written for a deleted
	// server surfaces as ErrNotFound.
	return updateColumn(ctx, s.db, (*McpServerConfig)(nil), "mcp server oauth token", id, "oauth_token", sealSecret(labelMcpOAuthToken, tokenJSON))
}

// ClearOAuthToken removes the persisted OAuth token for the given server.
func (s *McpServerStore) ClearOAuthToken(ctx context.Context, id string) error {
	return s.SaveOAuthToken(ctx, id, "")
}
