package store

import "github.com/uptrace/bun"

// McpServerStore persists MCP server connection configs.
type McpServerStore struct {
	*CrudStore[McpServerConfig]
}

// NewMcpServerStore returns an McpServerStore backed by db.
func NewMcpServerStore(db *bun.DB) *McpServerStore {
	return &McpServerStore{NewCrudStore[McpServerConfig](db, "mcp server config", "updated_at DESC")}
}
