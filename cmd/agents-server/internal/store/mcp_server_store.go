package store

import (
	"context"

	"github.com/uptrace/bun"
)

// McpServerStore persists MCP server connection configs.
type McpServerStore struct {
	*CrudStore[McpServerConfig]
}

// NewMcpServerStore returns an McpServerStore backed by db. Names are unique
// per scope (partial indexes, decisions §5.29); a duplicate is a UNIQUE error.
func NewMcpServerStore(db *bun.DB) *McpServerStore {
	return &McpServerStore{NewCrudStore[McpServerConfig](db, "mcp server config").withSecrets(sealMcpServer, openMcpServer)}
}

// Create inserts the server config, its credentials sealed.
func (s *McpServerStore) Create(ctx context.Context, m *McpServerConfig) error {
	return s.create(ctx, m)
}

// DeleteOwnedBy removes the server config while it still belongs to expectOwner.
func (s *McpServerStore) DeleteOwnedBy(ctx context.Context, id, expectOwner string) error {
	return deleteOwnedBy[McpServerConfig](ctx, s.db, s.label, id, expectOwner)
}

// SetScope moves the server config between global and private; expectOwner is the
// owner the caller authorized against.
func (s *McpServerStore) SetScope(ctx context.Context, id, scope, expectOwner string) error {
	return setScopeOf(ctx, s.CrudStore, id, scope, expectOwner)
}

// SetOwner transfers the server config to another user.
func (s *McpServerStore) SetOwner(ctx context.Context, id, ownerID string) error {
	return setOwnerOf(ctx, s.CrudStore, id, ownerID)
}

// Update overwrites the server config; the stored row is read in the same
// transaction and handed to prepare (nil to skip), oauth_token already copied
// onto m. ErrNotFound when absent.
func (s *McpServerStore) Update(ctx context.Context, id string, m *McpServerConfig, prepare func(prev *McpServerConfig) error) error {
	return s.updateInTx(ctx, id, m, func(prev *McpServerConfig) error {
		m.OAuthToken = prev.OAuthToken
		if prepare == nil {
			return nil
		}
		return prepare(prev)
	})
}

// SaveOAuthToken persists the serialized OAuth token for the given server,
// updating only the oauth_token column.
func (s *McpServerStore) SaveOAuthToken(ctx context.Context, id, tokenJSON string) error {
	// A token written for a deleted server is ErrNotFound (updateColumn).
	return updateColumn(ctx, s.db, (*McpServerConfig)(nil), "mcp server oauth token", id, "oauth_token", sealSecret(labelMcpOAuthToken, tokenJSON))
}

// ClearOAuthToken removes the persisted OAuth token for the given server.
func (s *McpServerStore) ClearOAuthToken(ctx context.Context, id string) error {
	return s.SaveOAuthToken(ctx, id, "")
}
