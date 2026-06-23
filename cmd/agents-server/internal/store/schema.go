package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// CreateSchema creates every table and supporting index if they do not already exist.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	models := []any{
		(*Session)(nil),
		(*Message)(nil),
		(*AgentConfig)(nil),
		(*McpServerConfig)(nil),
		(*Memory)(nil),
		(*Setting)(nil),
		(*ProviderRoute)(nil),
		(*SandboxConfig)(nil),
		(*TraceEvent)(nil),
	}
	for _, model := range models {
		if _, err := db.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating table for %T: %w", model, err)
		}
	}
	if _, err := db.NewCreateIndex().
		Model((*Message)(nil)).
		Index("idx_messages_session_id").
		Column("session_id", "id").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating messages index: %w", err)
	}
	if _, err := db.NewCreateIndex().
		Model((*TraceEvent)(nil)).
		Index("idx_trace_events_session_run").
		Column("session_id", "run_id").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating trace_events index: %w", err)
	}
	return nil
}
