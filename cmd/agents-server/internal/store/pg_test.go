package store

import (
	"context"
	"errors"
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

// The attachment lifecycle on PostgreSQL: uuid-typed columns, bun.List IN
// clauses and the bool/timestamp comparisons all behave as the SQLite tests
// established.
func TestPGAttachmentStore(t *testing.T) {
	ctx := context.Background()
	s := NewAttachmentStore(pgTestDB(t))

	a := &Attachment{OwnerID: NewID(), Key: "att/a.png", Mime: "image/png", Size: 10}
	if err := s.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	meta, err := s.MetaBatch(ctx, []string{a.ID, NewID()})
	if err != nil || len(meta) != 1 {
		t.Fatalf("MetaBatch = %v, %v", meta, err)
	}
	if err := s.MarkBound(ctx, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, a.ID); got == nil || !got.Bound {
		t.Fatal("not bound")
	}
	orphan := &Attachment{OwnerID: NewID(), Key: "att/o.png", Mime: "image/png", Size: 1}
	if err := s.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	old, err := s.ListUnboundBefore(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil || len(old) != 1 || old[0].ID != orphan.ID {
		t.Fatalf("orphans = %v, %v", old, err)
	}
	if err := s.Delete(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, orphan.ID); err != nil {
		t.Fatal(err) // idempotent for the reaper's retry
	}
}

// The instance lock admits one holder per PostgreSQL database (the lock is
// database-wide): while one connection holds it, a second acquire is refused,
// and after release the next acquire succeeds.
func TestPGInstanceLockIsExclusive(t *testing.T) {
	ctx := context.Background()
	db1 := pgTestDB(t)
	db2 := pgTestDB(t) // a different schema, the same database

	release1, err := AcquireInstanceLock(ctx, db1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireInstanceLock(ctx, db2); !errors.Is(err, ErrInstanceLocked) {
		t.Fatalf("second acquire = %v, want ErrInstanceLocked", err)
	}
	release1()

	release2, err := AcquireInstanceLock(ctx, db2)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}
