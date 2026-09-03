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
	(*Skill)(nil),
	(*Memory)(nil),
	(*Setting)(nil),
	(*Provider)(nil),
	(*Workflow)(nil),
	(*Trigger)(nil),
	(*Sandbox)(nil),
	(*Project)(nil),
	(*TraceEvent)(nil),
	(*TraceBlob)(nil),
	(*Guardrail)(nil),
	(*PendingApproval)(nil),
	(*Task)(nil),
	(*Wakeup)(nil),
	(*ContextProfile)(nil),
	(*Attachment)(nil),
	(*User)(nil),
	(*Identity)(nil),
	(*AuthToken)(nil),
	(*AuditEvent)(nil),
}

// CreateSchema creates every table and supporting index if they do not
// already exist, then verifies the result is the schema this build expects (verifySchema).
func CreateSchema(ctx context.Context, db *bun.DB) error {
	for _, model := range schemaModels {
		if _, err := db.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating table for %T: %w", model, err)
		}
	}
	if err := verifySchema(ctx, db); err != nil {
		return err
	}
	// Entry rows are addressed by (session, generation). Both indexes are
	// UNIQUE and load-bearing: seqs and entry ids are never issued twice (spec §2.5e2).
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
	// Trace events are read as "all spans of a session, ordered by id", so
	// index (session_id, id), not (session_id, run_id).
	if _, err := db.NewCreateIndex().
		Model((*TraceEvent)(nil)).
		Index("idx_trace_events_session_id").
		Column("session_id", "id").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating trace_events session index: %w", err)
	}
	// Task lookups run by parent (per chat turn) and by child session (per
	// run start) — index both edges, generation included.
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
	// The session list orders by recency OF CHANGE (spec §2.5e2, "the change
	// record"), so it sorts and indexes on updated_at, not created_at.
	if _, err := db.NewCreateIndex().
		Model((*Session)(nil)).
		Index("idx_sessions_updated_at").
		Column("updated_at").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating sessions updated_at index: %w", err)
	}
	// Scoped-entity names: unique per visibility context via two partial
	// indexes per table (decisions §5.29). HITL run state names agents by name.
	for _, t := range []struct {
		model any
		table string
	}{
		{(*AgentConfig)(nil), "agent_configs"},
		{(*Provider)(nil), "providers"},
		{(*McpServerConfig)(nil), "mcp_servers"},
	} {
		if _, err := db.NewCreateIndex().
			Model(t.model).
			Index("idx_" + t.table + "_name_global").
			Unique().
			Column("name").
			Where("scope = 'global'").
			IfNotExists().
			Exec(ctx); err != nil {
			return fmt.Errorf("creating %s global name index: %w", t.table, err)
		}
		if _, err := db.NewCreateIndex().
			Model(t.model).
			Index("idx_"+t.table+"_name_private").
			Unique().
			Column("owner_id", "name").
			Where("scope = 'private'").
			IfNotExists().
			Exec(ctx); err != nil {
			return fmt.Errorf("creating %s private name index: %w", t.table, err)
		}
	}
	// Skill uniqueness is per (visibility context, repo LABEL) — decisions
	// §5.31. COALESCE because NULLs never collide in a unique index.
	if _, err := db.NewCreateIndex().
		Model((*Skill)(nil)).
		Index("idx_skills_name_global").
		Unique().
		ColumnExpr("COALESCE(repo_label, ''), name").
		Where("scope = 'global'").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating skills global name index: %w", err)
	}
	if _, err := db.NewCreateIndex().
		Model((*Skill)(nil)).
		Index("idx_skills_name_private").
		Unique().
		ColumnExpr("owner_id, COALESCE(repo_label, ''), name").
		Where("scope = 'private'").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating skills private name index: %w", err)
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
	// Workflow names follow the same per-scope rule, case-insensitively (the
	// tool matches names with EqualFold).
	workflowName := "name COLLATE NOCASE"
	if db.Dialect().Name() == dialect.PG {
		workflowName = "lower(name)"
	}
	if _, err := db.NewCreateIndex().
		Model((*Workflow)(nil)).
		Index("idx_workflows_name_global").
		Unique().
		ColumnExpr(workflowName).
		Where("scope = 'global'").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating workflows global name index: %w", err)
	}
	if _, err := db.NewCreateIndex().
		Model((*Workflow)(nil)).
		Index("idx_workflows_name_private").
		Unique().
		ColumnExpr("owner_id, " + workflowName).
		Where("scope = 'private'").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating workflows private name index: %w", err)
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
	// A project's name is how a person picks it per (owner, sandbox); two
	// sharing one make the choice a coin flip.
	if _, err := db.NewCreateIndex().
		Model((*Project)(nil)).
		Index("idx_projects_owner_sandbox_name").
		Unique().
		Column("owner_id", "sandbox_id", "name").
		IfNotExists().
		Exec(ctx); err != nil {
		return fmt.Errorf("creating projects unique name index: %w", err)
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
	// Accounts merge by verified email, so email is an identity; UNIQUE also
	// arbitrates two first logins racing to create the same account.
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

// verifySchema probes every model with a zero-row SELECT, so a database of
// another shape fails at startup — invariant 25.
func verifySchema(ctx context.Context, db *bun.DB) error {
	for _, model := range schemaModels {
		// The slice destination makes zero rows a valid result (the nil model
		// itself would demand exactly one row).
		var probe []map[string]any
		if err := db.NewSelect().Model(model).Limit(0).Scan(ctx, &probe); err != nil {
			return fmt.Errorf(
				"database schema is out of date for %T (%w); this build changed the database layout and ships no migrations — back up the database if needed, delete it (or drop its tables), and restart to recreate it",
				model, err)
		}
	}
	return nil
}
