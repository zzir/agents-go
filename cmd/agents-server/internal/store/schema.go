package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// schemaModels is every persisted model — the tables CreateSchema creates and
// verifySchema probes.
var schemaModels = []any{
	(*Session)(nil),
	(*entryRow)(nil),
	(*appendPointRow)(nil),
	(*AgentConfig)(nil),
	(*McpServerConfig)(nil),
	(*Memory)(nil),
	(*Setting)(nil),
	(*Provider)(nil),
	(*Workflow)(nil),
	(*Trigger)(nil),
	(*SandboxConfig)(nil),
	(*TraceEvent)(nil),
	(*Guardrail)(nil),
	(*PendingApproval)(nil),
	(*Task)(nil),
	(*Wakeup)(nil),
	(*ContextProfile)(nil),
	(*User)(nil),
	(*Identity)(nil),
	(*AuthToken)(nil),
	(*AuditEvent)(nil),
}

// CreateSchema creates every table and supporting index if they do not
// already exist, then verifies the result is the schema this build expects
// (see verifySchema — IF NOT EXISTS skips a table that exists in an older
// shape).
func CreateSchema(ctx context.Context, db *bun.DB) error {
	for _, model := range schemaModels {
		if _, err := db.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating table for %T: %w", model, err)
		}
	}
	if err := verifySchema(ctx, db); err != nil {
		return err
	}
	// Entry reads and writes are addressed by (session, generation) — the gen
	// belongs in every key. Both indexes are UNIQUE, and that is load-bearing:
	// sequence numbers and entry ids are never handed out twice (spec §2.5e2),
	// and a backend that can constrain them does, so a race or a bug that
	// would mint a duplicate becomes a failed write instead of two rows
	// answering to one name. No migration — rebuild the database.
	if _, err := db.NewCreateIndex().
		Model((*entryRow)(nil)).
		Index("idx_entries_session_seq").
		Unique().
		Column("session_id", "gen", "seq").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating entries index: %w", err)
	}
	// Point lookups by entry id: resolving one entry would otherwise read the
	// whole session.
	if _, err := db.NewCreateIndex().
		Model((*entryRow)(nil)).
		Index("idx_entries_entry_id").
		Unique().
		Column("session_id", "gen", "entry_id").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating entries entry_id index: %w", err)
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
	// Task lookups run per chat turn (list by parent) and on every run start
	// (taskMeta by child session) — index both edges. The generation is part
	// of each key because it is part of every one of those lookups: a task row
	// belongs to one generation of a session id, not to the id.
	if _, err := db.NewCreateIndex().
		Model((*Task)(nil)).
		Index("idx_tasks_parent_session_id").
		Column("parent_session_id", "parent_session_gen").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating tasks parent index: %w", err)
	}
	if _, err := db.NewCreateIndex().
		Model((*Task)(nil)).
		Index("idx_tasks_child_session_id").
		Column("child_session_id", "child_session_gen").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating tasks child index: %w", err)
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
	// The session list orders by recency OF CHANGE: every append, pop and
	// clear moves a session (spec §2.5e2, "the change record"), so the list
	// sorts and indexes on updated_at, not created_at.
	if _, err := db.NewCreateIndex().
		Model((*Session)(nil)).
		Index("idx_sessions_updated_at").
		Column("updated_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating sessions updated_at index: %w", err)
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
		Index("idx_guardrails_name").
		Unique().
		Column("name").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating guardrails unique name index: %w", err)
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
	// A workflow's name is how a person picks it to run; two sharing one make
	// the choice a coin flip. Case-insensitive because the tool matches the
	// name with EqualFold — "Build" and "build" must not both exist, or the
	// model naming one hits whichever the listing happens to return.
	workflowName := "name COLLATE NOCASE"
	if db.Dialect().Name() == dialect.PG {
		workflowName = "lower(name)"
	}
	if _, err := db.NewCreateIndex().
		Model((*Workflow)(nil)).
		Index("idx_workflows_name").
		Unique().
		ColumnExpr(workflowName).
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating workflows unique name index: %w", err)
	}
	// Draining asks for one session's debts, and the restart sweep asks for
	// every session owed one; the hourly prune asks for the settled ones by age.
	if _, err := db.NewCreateIndex().
		Model((*Wakeup)(nil)).
		Index("idx_wakeups_session_state").
		Column("session_id", "state").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating wakeups index: %w", err)
	}
	if _, err := db.NewCreateIndex().
		Model((*Wakeup)(nil)).
		Index("idx_wakeups_state_created").
		Column("state", "created_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating wakeups prune index: %w", err)
	}
	// A provider's name is how a person picks it in every config UI, so two
	// sharing one make the choice a coin flip. Enforce it at the DB.
	if _, err := db.NewCreateIndex().
		Model((*Provider)(nil)).
		Index("idx_providers_name").
		Unique().
		Column("name").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating providers unique name index: %w", err)
	}
	// The sidebar lists one owner's sessions by recency of change.
	if _, err := db.NewCreateIndex().
		Model((*Session)(nil)).
		Index("idx_sessions_owner_updated_at").
		Column("owner_id", "updated_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating sessions owner index: %w", err)
	}
	// Accounts merge by verified email, so email is an identity — two rows
	// sharing one would make every merge a coin flip. UNIQUE also arbitrates
	// two first logins racing to create the same account.
	if _, err := db.NewCreateIndex().
		Model((*User)(nil)).
		Index("idx_users_email").
		Unique().
		Column("email").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating users unique email index: %w", err)
	}
	// One provider subject is one login; UNIQUE arbitrates concurrent logins
	// of the same subject racing to link it.
	if _, err := db.NewCreateIndex().
		Model((*Identity)(nil)).
		Index("idx_identities_subject").
		Unique().
		Column("provider", "subject").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating identities unique subject index: %w", err)
	}
	// The audit log is read newest-first and pruned by age.
	if _, err := db.NewCreateIndex().
		Model((*AuditEvent)(nil)).
		Index("idx_audit_events_created_at").
		Column("created_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating audit_events created_at index: %w", err)
	}
	// Every request authenticates by hash lookup — this index IS the auth path.
	if _, err := db.NewCreateIndex().
		Model((*AuthToken)(nil)).
		Index("idx_auth_tokens_hash").
		Unique().
		Column("token_hash").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating auth_tokens unique hash index: %w", err)
	}
	return nil
}

// verifySchema probes every model with a zero-row SELECT. bun names every
// mapped column in the SELECT list, so the model definitions themselves are the
// probe — a database created by an older build fails here at startup, with one
// clear message, instead of per-request. There is deliberately no schema-version
// constant: the models are the version, and schema changes ship without
// migrations (the remedy is recreating the file).
func verifySchema(ctx context.Context, db *bun.DB) error {
	for _, model := range schemaModels {
		// The slice destination makes zero rows a valid result; scanning into
		// the nil model itself would demand exactly one row and report the
		// empty table as sql.ErrNoRows.
		var probe []map[string]any
		if err := db.NewSelect().Model(model).Limit(0).Scan(ctx, &probe); err != nil {
			return fmt.Errorf(
				"database schema is out of date for %T (%w); this build changed the database layout and ships no migrations — back up the database if needed, delete it (or drop its tables), and restart to recreate it",
				model, err)
		}
	}
	return nil
}
