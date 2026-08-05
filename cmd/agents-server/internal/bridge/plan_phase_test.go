package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// countPlanMarkers counts persisted plan_unlocked annotations for a session.
func countPlanMarkers(t *testing.T, sa *store.EntryStore, ref session.Ref) int {
	t.Helper()
	views, err := sa.GetEntries(context.Background(), ref, 0, 0)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	n := 0
	for _, v := range views {
		if v.Kind == "annotation" && v.Display != nil && v.Display.Kind == store.PlanUnlockedKind {
			n++
		}
	}
	return n
}

// The write half of the marker chain: persisting the marker is the unlock's
// precondition, it lands exactly once, and a failed write keeps the phase
// locked instead of letting behavior run ahead of the durable record.
func TestPlanUnlockMarkerPersistence(t *testing.T) {
	db := newTestDB(t)
	sessionID := store.NewID()
	sa := store.NewEntryStoreFor(db, session.Direct(sessionID))
	sa.SetRunID("r1")

	phase := &middleware.PlanPhase{}
	armPlanUnlock(phase, sa)
	if err := phase.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if !phase.Executing() {
		t.Fatal("phase must be executing after a successful unlock")
	}
	if got, err := sa.RunHasAnnotation(context.Background(), "r1", store.PlanUnlockedKind); err != nil || !got {
		t.Fatalf("marker not found after unlock (err=%v)", err)
	}
	// A second unlock is a no-op: still exactly one marker.
	if err := phase.Unlock(); err != nil {
		t.Fatalf("idempotent unlock: %v", err)
	}
	if n := countPlanMarkers(t, sa, session.Ref{ID: sessionID}); n != 1 {
		t.Fatalf("marker rows = %d, want 1", n)
	}

	// A failed write fails the unlock and the phase stays planning.
	db2 := newTestDB(t)
	sa2 := store.NewEntryStoreFor(db2, session.Direct(store.NewID()))
	sa2.SetRunID("r2")
	if err := db2.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	phase2 := &middleware.PlanPhase{}
	armPlanUnlock(phase2, sa2)
	if err := phase2.Unlock(); err == nil {
		t.Fatal("a failed marker write must fail the unlock")
	}
	if phase2.Executing() {
		t.Fatal("the phase must stay locked when the marker write fails")
	}
}

// The read half: restorePlanPhase puts a rebuilt run into the phase its
// marker says, replaying without duplicating, and a failed read is an error
// (the pending approval is unclaimed at that point — the decision retries)
// rather than a silent fall-back into planning.
func TestRestorePlanPhase(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:   store.NewAgentConfigStore(db),
		Settings:       store.NewSettingStore(db),
		Memories:       store.NewMemoryStore(db),
		McpServers:     store.NewMcpServerStore(db),
		ProviderRoutes: store.NewProviderRouteStore(db),
		Guardrails:     NewGuardrailResolver(store.NewGuardrailStore(db)),
		SandboxManager: NewSandboxManager(t.TempDir()),
	})

	// No marker: the rebuilt run stays planning, and the armed hook persists
	// the marker when THIS resume unlocks.
	phase := &middleware.PlanPhase{}
	if err := runner.restorePlanPhase(ctx, phase, sess.ID, "r1"); err != nil {
		t.Fatalf("restore (no marker): %v", err)
	}
	if phase.Executing() {
		t.Fatal("no marker must mean planning phase")
	}
	if err := phase.Unlock(); err != nil {
		t.Fatalf("unlock after restore: %v", err)
	}

	// Marker present: a fresh rebuild restores straight into executing, and
	// the replayed unlock does not write a second marker.
	phase2 := &middleware.PlanPhase{}
	if err := runner.restorePlanPhase(ctx, phase2, sess.ID, "r1"); err != nil {
		t.Fatalf("restore (marker present): %v", err)
	}
	if !phase2.Executing() {
		t.Fatal("the marker must restore the executing phase")
	}
	ref, err := store.RefFor(ctx, db, sess.ID)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	if n := countPlanMarkers(t, store.NewEntryStoreFor(db, ref), ref); n != 1 {
		t.Fatalf("marker rows after replayed restore = %d, want 1", n)
	}

	// A failed read surfaces as an error — never a silent planning phase.
	dbBroken := newTestDB(t)
	sessions2 := store.NewSessionStore(dbBroken)
	sess2 := &store.Session{ID: store.NewID(), Name: "s2"}
	if err := sessions2.Create(ctx, sess2); err != nil {
		t.Fatalf("create session: %v", err)
	}
	broken := NewRunner(ctx, dbBroken, &AgentDeps{
		AgentConfigs:   store.NewAgentConfigStore(dbBroken),
		Settings:       store.NewSettingStore(dbBroken),
		Memories:       store.NewMemoryStore(dbBroken),
		McpServers:     store.NewMcpServerStore(dbBroken),
		ProviderRoutes: store.NewProviderRouteStore(dbBroken),
		Guardrails:     NewGuardrailResolver(store.NewGuardrailStore(dbBroken)),
		SandboxManager: NewSandboxManager(t.TempDir()),
	})
	if err := dbBroken.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := broken.restorePlanPhase(ctx, &middleware.PlanPhase{}, sess2.ID, "r1"); err == nil {
		t.Fatal("a failed marker read must abort the resolve, not fall back to planning")
	}
}
