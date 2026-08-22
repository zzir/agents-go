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

// NewMcpServerStore returns an McpServerStore backed by db. Server-name
// uniqueness (the name is the tool-prefix namespace) is enforced by the DB
// (idx_mcp_servers_name); a duplicate surfaces as a UNIQUE-constraint error
// that handlers map to 409.
func NewMcpServerStore(db *bun.DB) *McpServerStore {
	return &McpServerStore{NewCrudStore[McpServerConfig](db, "mcp server config", "created_at DESC").withSecrets(sealMcpServer, openMcpServer)}
}

// Update overwrites the server config but preserves the oauth_token column.
// Returns an ErrNotFound-wrapping error when the row doesn't exist.
func (s *McpServerStore) Update(ctx context.Context, id string, m *McpServerConfig) error {
	err := sealedWrite(m, sealMcpServer, openMcpServer, func() error {
		res, err := s.db.NewUpdate().Model(m).
			ExcludeColumn("id", "created_at", "oauth_token").
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("updating mcp server config %s: %w", id, err)
	}
	return nil
}

// SaveOAuthToken persists the serialized OAuth token for the given server,
// updating only the oauth_token column so it is not cleared by regular CRUD
// updates.
func (s *McpServerStore) SaveOAuthToken(ctx context.Context, id, tokenJSON string) error {
	// updateColumn enforces the row exists, so a token written for a deleted
	// server surfaces as ErrNotFound instead of a silent no-op the OAuth flow
	// would mistake for success.
	return updateColumn(ctx, s.db, (*McpServerConfig)(nil), "mcp server oauth token", id, "oauth_token", sealSecret(labelMcpOAuthToken, tokenJSON))
}

// ClearOAuthToken removes the persisted OAuth token for the given server.
func (s *McpServerStore) ClearOAuthToken(ctx context.Context, id string) error {
	return s.SaveOAuthToken(ctx, id, "")
}
