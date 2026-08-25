package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

func TestPGEntryStoreConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(t *testing.T) session.Storage {
		t.Helper()
		return NewEntryStoreFor(pgTestDB(t), session.Direct(NewID()))
	})
}

func TestPGRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(t *testing.T) agentstest.RepoUnderTest {
		t.Helper()
		db := pgTestDB(t)
		sessions := NewSessionStore(db)
		// Every id column is uuid-typed on PostgreSQL: the suite's literal
		// names become memoized UUIDs.
		ids := map[string]string{}
		return agentstest.RepoUnderTest{
			Repo: NewSessionRepoAdapter(sessions, func(ref session.Ref) session.Storage {
				return NewEntryStoreFor(db, ref)
			}),
			IDs: func(name string) string {
				if id, ok := ids[name]; ok {
					return id
				}
				ids[name] = NewID()
				return ids[name]
			},
		}
	})
}

// A duplicate insert must map to a 409-able UniqueViolation on PostgreSQL just
// as it does on SQLite, with the offending column named.
func TestPGUniqueViolation(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	agents := NewAgentConfigStore(db)
	if err := agents.Create(ctx, &AgentConfig{Name: "dup", Scope: ScopeGlobal, OwnerID: NewID()}); err != nil {
		t.Fatal(err)
	}
	err := agents.Create(ctx, &AgentConfig{Name: "dup", Scope: ScopeGlobal, OwnerID: NewID()})
	if err == nil {
		t.Fatal("duplicate agent name inserted without error")
	}
	cols, ok := UniqueViolation(err)
	if !ok || !strings.Contains(cols, "name") {
		t.Fatalf("UniqueViolation(%v) = %q, %v; want the name column reported", err, cols, ok)
	}
}

// The summary listing's JSON surgery is dialect-specific SQL — prove the
// PostgreSQL branch strips payload fields, flags the row, and leaves the full
// row reachable by span.
func TestPGTraceSummary(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	traces := NewTraceStore(db)
	sessionID, runID := NewID(), NewID()
	ev := &TraceEvent{
		SessionID: sessionID, RunID: runID, Kind: "span", SpanID: "sp1", Name: "generation",
		Data:      `{"model":"gpt","input":[{"role":"user"}],"output":"big"}`,
		CreatedAt: time.Now().UTC(),
	}
	if err := traces.Insert(ctx, ev); err != nil {
		t.Fatal(err)
	}
	rows, err := traces.ListSummaryBySession(ctx, sessionID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].PayloadOmitted {
		t.Error("PayloadOmitted = false, want true for a row with payload fields")
	}
	if strings.Contains(rows[0].Data, "input") || !strings.Contains(rows[0].Data, "model") {
		t.Errorf("summary data = %q, want payload stripped and the rest kept", rows[0].Data)
	}
	full, err := traces.GetBySpan(ctx, sessionID, "sp1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Data, "input") {
		t.Errorf("GetBySpan data = %q, want the payload intact", full.Data)
	}
}

// Two workflows whose names differ only by case must collide on PostgreSQL
// (lower(name) unique index) as they do on SQLite (COLLATE NOCASE).
func TestPGWorkflowNameCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	workflows := NewWorkflowStore(db)
	if err := workflows.Create(ctx, &Workflow{Name: "Build", Scope: ScopeGlobal, OwnerID: NewID()}); err != nil {
		t.Fatal(err)
	}
	err := workflows.Create(ctx, &Workflow{Name: "build", Scope: ScopeGlobal, OwnerID: NewID()})
	if err == nil {
		t.Fatal("case-variant workflow name inserted without error")
	}
	if _, ok := UniqueViolation(err); !ok {
		t.Fatalf("want a UniqueViolation, got %v", err)
	}
}
