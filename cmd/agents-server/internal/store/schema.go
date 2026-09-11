package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// already exist, then verifies the tables (verifySchema) and the unique
// indexes (verifyIndexes) are the shape this build expects.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	for _, model := range schemaModels {
		if _, err := db.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating table for %T: %w", model, err)
		}
	}
	if err := verifySchema(ctx, db); err != nil {
		return err
	}
	pg := db.Dialect().Name() == dialect.PG
	indexes := schemaIndexes(pg)
	for _, ix := range indexes {
		q := db.NewCreateIndex().Model(ix.model).Index(ix.name).IfNotExists()
		if ix.unique {
			q = q.Unique()
		}
		if ix.expr != "" {
			q = q.ColumnExpr(ix.expr)
		} else {
			q = q.Column(ix.columns...)
		}
		if ix.where != "" {
			q = q.Where(ix.where)
		}
		if _, err := q.Exec(ctx); err != nil {
			return fmt.Errorf("creating index %s: %w", ix.name, err)
		}
	}
	return verifyIndexes(ctx, db, indexes)
}

// schemaIndex is one supporting index, created IF NOT EXISTS and, when
// unique, probed by shape at startup (verifyIndexes).
type schemaIndex struct {
	model  any
	name   string
	unique bool
	// columns are the indexed columns in order. With expr set, creation uses
	// that raw expression and columns are the identifiers the probe expects in it.
	columns []string
	expr    string
	where   string
}

// schemaIndexes lists every supporting index; the case-insensitive workflow
// name is the one dialect-specific expression.
func schemaIndexes(pg bool) []schemaIndex {
	workflowName := "name COLLATE NOCASE"
	if pg {
		workflowName = "lower(name)"
	}
	return []schemaIndex{
		// Entry rows are addressed by (session, generation); both indexes are
		// UNIQUE and load-bearing: seqs and entry ids are never issued twice (spec §2.5e2).
		{model: (*entryRow)(nil), name: "idx_entries_session_seq", unique: true, columns: []string{"session_id", "gen", "seq"}},
		// Point lookups by entry id, else resolving one entry reads the session.
		{model: (*entryRow)(nil), name: "idx_entries_entry_id", unique: true, columns: []string{"session_id", "gen", "entry_id"}},
		// Trace events are read as "all spans of a session, ordered by id".
		{model: (*TraceEvent)(nil), name: "idx_trace_events_session_id", columns: []string{"session_id", "id"}},
		// Task lookups run by parent (per chat turn) and by child session (per
		// run start), generation included.
		{model: (*Task)(nil), name: "idx_tasks_parent_session_id", columns: []string{"parent_session_id", "parent_session_gen"}},
		{model: (*Task)(nil), name: "idx_tasks_child_session_id", columns: []string{"child_session_id", "child_session_gen"}},
		// Trace retention prunes by age; without this the periodic DELETE
		// full-scans the largest table.
		{model: (*TraceEvent)(nil), name: "idx_trace_events_created_at", columns: []string{"created_at"}},
		// Memories are read by scope; the key is unique within one, which is
		// what lets a write be an upsert.
		{model: (*Memory)(nil), name: "idx_memories_scope", columns: []string{"scope_kind", "scope_id"}},
		{model: (*Memory)(nil), name: "idx_memories_scope_key", unique: true, columns: []string{"scope_kind", "scope_id", "gen", "key"}},
		// The session list orders by recency OF CHANGE (spec §2.5e2), per
		// owner in the sidebar; a project's sessions are counted before its delete.
		{model: (*Session)(nil), name: "idx_sessions_updated_at", columns: []string{"updated_at"}},
		{model: (*Session)(nil), name: "idx_sessions_owner_updated_at", columns: []string{"owner_id", "updated_at"}},
		{model: (*Session)(nil), name: "idx_sessions_project_id", columns: []string{"project_id"}},
		// Scoped-entity names: unique per visibility context, two partial
		// indexes per table (decisions §5.29). HITL run state names agents by name.
		{model: (*AgentConfig)(nil), name: "idx_agent_configs_name_global", unique: true, columns: []string{"name"}, where: "scope = 'global'"},
		{model: (*AgentConfig)(nil), name: "idx_agent_configs_name_private", unique: true, columns: []string{"owner_id", "name"}, where: "scope = 'private'"},
		{model: (*Provider)(nil), name: "idx_providers_name_global", unique: true, columns: []string{"name"}, where: "scope = 'global'"},
		{model: (*Provider)(nil), name: "idx_providers_name_private", unique: true, columns: []string{"owner_id", "name"}, where: "scope = 'private'"},
		{model: (*McpServerConfig)(nil), name: "idx_mcp_servers_name_global", unique: true, columns: []string{"name"}, where: "scope = 'global'"},
		{model: (*McpServerConfig)(nil), name: "idx_mcp_servers_name_private", unique: true, columns: []string{"owner_id", "name"}, where: "scope = 'private'"},
		// Skill uniqueness is per (visibility context, repo LABEL) — decisions
		// §5.31. COALESCE because NULLs never collide in a unique index.
		{model: (*Skill)(nil), name: "idx_skills_name_global", unique: true, expr: "COALESCE(repo_label, ''), name", columns: []string{"repo_label", "name"}, where: "scope = 'global'"},
		{model: (*Skill)(nil), name: "idx_skills_name_private", unique: true, expr: "owner_id, COALESCE(repo_label, ''), name", columns: []string{"owner_id", "repo_label", "name"}, where: "scope = 'private'"},
		// Agents reference guardrails by name; a duplicate would make the
		// reference order-dependent.
		{model: (*Guardrail)(nil), name: "idx_guardrails_name", unique: true, columns: []string{"name"}},
		// Workflow names follow the per-scope rule, case-insensitively (the
		// tool matches names with EqualFold).
		{model: (*Workflow)(nil), name: "idx_workflows_name_global", unique: true, expr: workflowName, columns: []string{"name"}, where: "scope = 'global'"},
		{model: (*Workflow)(nil), name: "idx_workflows_name_private", unique: true, expr: "owner_id, " + workflowName, columns: []string{"owner_id", "name"}, where: "scope = 'private'"},
		// Draining asks for one session's debts, the restart sweep for every
		// session owed one, the hourly prune for the settled ones by age.
		{model: (*Wakeup)(nil), name: "idx_wakeups_session_state", columns: []string{"session_id", "state"}},
		{model: (*Wakeup)(nil), name: "idx_wakeups_state_created", columns: []string{"state", "created_at"}},
		// A project's name is how a person picks it per (owner, sandbox).
		{model: (*Project)(nil), name: "idx_projects_owner_sandbox_name", unique: true, columns: []string{"owner_id", "sandbox_id", "name"}},
		// Accounts merge by verified email; UNIQUE also arbitrates two first
		// logins racing to create the same account.
		{model: (*User)(nil), name: "idx_users_email", unique: true, columns: []string{"email"}},
		// One provider subject is one login; UNIQUE arbitrates concurrent logins.
		{model: (*Identity)(nil), name: "idx_identities_subject", unique: true, columns: []string{"provider", "subject"}},
		// The audit log is read newest-first and pruned by age.
		{model: (*AuditEvent)(nil), name: "idx_audit_events_created_at", columns: []string{"created_at"}},
		// Every request authenticates by hash lookup — this index IS the auth path.
		{model: (*AuthToken)(nil), name: "idx_auth_tokens_hash", unique: true, columns: []string{"token_hash"}},
	}
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

// verifyIndexes reads every UNIQUE index's definition from the catalog and
// checks its shape — uniqueness, the indexed identifiers in order, the
// partial predicate's literal — since CREATE INDEX IF NOT EXISTS keeps an
// index of an older shape — invariant 25.
func verifyIndexes(ctx context.Context, db *bun.DB, indexes []schemaIndex) error {
	defs, err := indexDefinitions(ctx, db)
	if err != nil {
		return fmt.Errorf("reading the index catalog: %w", err)
	}
	for _, ix := range indexes {
		if !ix.unique {
			continue
		}
		if err := ix.matches(defs[ix.name]); err != nil {
			return fmt.Errorf(
				"database schema is out of date: index %s %v; this build changed the database layout and ships no migrations — back up the database if needed, delete it (or drop its tables), and restart to recreate it",
				ix.name, err)
		}
	}
	return nil
}

// indexDefinitions returns the catalog's CREATE INDEX statement per index
// name, for the current schema.
func indexDefinitions(ctx context.Context, db *bun.DB) (map[string]string, error) {
	query := `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`
	if db.Dialect().Name() == dialect.PG {
		query = `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	defs := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return nil, err
		}
		defs[name] = def
	}
	return defs, rows.Err()
}

// matches reports why def is not this index's shape, nil when it is.
func (ix schemaIndex) matches(def string) error {
	if def == "" {
		return errors.New("is missing")
	}
	d := strings.ToLower(def)
	if !strings.Contains(d, "unique") {
		return errors.New("is not unique")
	}
	// The indexed list starts at the first parenthesis after the table name.
	body := d
	if i := strings.Index(d, "("); i >= 0 {
		body = d[i:]
	}
	pos := 0
	for _, col := range ix.columns {
		i := identIndex(body[pos:], col)
		if i < 0 {
			return fmt.Errorf("does not index %s in (%s)", col, strings.Join(ix.columns, ", "))
		}
		pos += i + len(col)
	}
	if ix.where != "" {
		if lit := ix.where[strings.Index(ix.where, "'"):]; !strings.Contains(body, lit) {
			return fmt.Errorf("is not partial on %s", ix.where)
		}
	}
	return nil
}

// identIndex is strings.Index for an identifier: the match may not touch
// another identifier character on either side.
func identIndex(s, ident string) int {
	isIdent := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
	}
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], ident)
		if i < 0 {
			return -1
		}
		i += from
		end := i + len(ident)
		if (i == 0 || !isIdent(s[i-1])) && (end == len(s) || !isIdent(s[end])) {
			return i
		}
		from = i + 1
	}
	return -1
}
