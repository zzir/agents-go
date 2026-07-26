package store

import (
	"context"
	"testing"
)

// TestSchemaIndexes documents and locks the intended index design: the unique
// constraints that back business identity, and the query indexes for the two
// tables that grow with history (messages, trace_events).
func TestSchemaIndexes(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t) // runs CreateSchema

	type idx struct {
		name   string
		unique bool
		cols   string
	}
	want := []idx{
		// Business-identity unique keys.
		{"idx_agent_configs_name", true, "name"},
		{"idx_mcp_servers_name", true, "name"},
		{"idx_guardrails_type_name", true, "type,name"},
		{"idx_provider_routes_prefix", true, "prefix"},
		// Query indexes for the history tables and hot lookups.
		{"idx_entries_session_id", false, "session_id,id"},
		{"idx_entries_entry_id", false, "session_id,entry_id"},
		{"idx_trace_events_session_id", false, "session_id,id"},
		{"idx_trace_events_created_at", false, "created_at"},
		{"idx_memories_agent_config_id", false, "agent_config_id"},
		{"idx_sessions_created_at", false, "created_at"},
	}

	for _, w := range want {
		t.Run(w.name, func(t *testing.T) {
			var unique bool
			// SQLite: index uniqueness via PRAGMA index_list.
			rows, err := db.QueryContext(ctx, "SELECT \"unique\" FROM pragma_index_list('"+tableForIndex(w.name)+"') WHERE name = ?", w.name)
			if err != nil {
				t.Fatalf("pragma index_list: %v", err)
			}
			found := false
			for rows.Next() {
				found = true
				if err := rows.Scan(&unique); err != nil {
					t.Fatalf("scan: %v", err)
				}
			}
			_ = rows.Close()
			if !found {
				t.Fatalf("index %s not found", w.name)
			}
			if unique != w.unique {
				t.Errorf("index %s unique = %v, want %v", w.name, unique, w.unique)
			}
		})
	}
}

// tableForIndex maps an index name to its table for PRAGMA lookups.
func tableForIndex(index string) string {
	switch index {
	case "idx_agent_configs_name":
		return "agent_configs"
	case "idx_mcp_servers_name":
		return "mcp_servers"
	case "idx_guardrails_type_name":
		return "guardrails"
	case "idx_provider_routes_prefix":
		return "provider_routes"
	case "idx_entries_session_id", "idx_entries_entry_id":
		return "entries"
	case "idx_trace_events_session_id", "idx_trace_events_created_at":
		return "trace_events"
	case "idx_memories_agent_config_id":
		return "memories"
	case "idx_sessions_created_at":
		return "sessions"
	}
	return ""
}
