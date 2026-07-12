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
		(*Guardrail)(nil),
		(*PendingApproval)(nil),
		(*Task)(nil),
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
	// Trace events are read as "all spans of a session, ordered by id" (the trace
	// panel groups by run_id client-side) — so index (session_id, id), not
	// (session_id, run_id): that serves the ORDER BY id directly, and the rare
	// fork query's `run_id IN (...)` is a cheap residual filter on top.
	if _, err := db.NewCreateIndex().
		Model((*TraceEvent)(nil)).
		Index("idx_trace_events_session_id").
		Column("session_id", "id").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating trace_events session index: %w", err)
	}
	// Trace retention prunes by age; without this the periodic DELETE
	// full-scans the largest table in the DB.
	if _, err := db.NewCreateIndex().
		Model((*TraceEvent)(nil)).
		Index("idx_trace_events_created_at").
		Column("created_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating trace_events created_at index: %w", err)
	}
	// Memories are loaded per agent (its own plus global "" scope).
	if _, err := db.NewCreateIndex().
		Model((*Memory)(nil)).
		Index("idx_memories_agent_config_id").
		Column("agent_config_id").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating memories agent index: %w", err)
	}
	// The session list orders by recency.
	if _, err := db.NewCreateIndex().
		Model((*Session)(nil)).
		Index("idx_sessions_created_at").
		Column("created_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating sessions created_at index: %w", err)
	}
	// Agent names must be unique — HITL run state serializes the current agent
	// by name, so a duplicate would resolve an approval resume to the wrong
	// config. Enforce it at the DB so concurrent writes and direct store access
	// can't produce duplicates behind the handler-level check.
	if _, err := db.NewCreateIndex().
		Model((*AgentConfig)(nil)).
		Index("idx_agent_configs_name").
		Unique().
		Column("name").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating agent_configs unique name index: %w", err)
	}
	// Guardrails are referenced by name within a type; a duplicate (type, name)
	// makes an agent's reference order-dependent. Enforce uniqueness at the DB.
	if _, err := db.NewCreateIndex().
		Model((*Guardrail)(nil)).
		Index("idx_guardrails_type_name").
		Unique().
		Column("type", "name").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating guardrails unique type/name index: %w", err)
	}
	// An MCP server's name is its tool-prefix namespace ("<name>__<tool>"), so
	// two servers sharing a name are ambiguous. The name is thus an identity —
	// enforce it unique at the DB (this also makes the agent-config validator's
	// cross-server name-collision check unreachable, since no two servers can
	// share a name).
	if _, err := db.NewCreateIndex().
		Model((*McpServerConfig)(nil)).
		Index("idx_mcp_servers_name").
		Unique().
		Column("name").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating mcp_servers unique name index: %w", err)
	}
	// Provider routes map a model-name prefix to credentials; a duplicate prefix
	// makes which credentials win order-dependent (last into the router map).
	if _, err := db.NewCreateIndex().
		Model((*ProviderRoute)(nil)).
		Index("idx_provider_routes_prefix").
		Unique().
		Column("prefix").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating provider_routes unique prefix index: %w", err)
	}
	return nil
}
