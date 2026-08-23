package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The write half: persisting the unlock (clearing the session's planning
// column) is the unlock's precondition, it is idempotent, and a failed write
// keeps the phase locked instead of letting behavior run ahead of the record.
func TestPlanUnlockClearsThePhase(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s", Planning: true}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	ref := session.Direct(sess.ID)
	sa := store.NewEntryStoreFor(db, ref)

	phase := &middleware.PlanPhase{}
	armPlanUnlock(phase, sa, ref)
	if err := phase.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if !phase.Executing() {
		t.Fatal("phase must be executing after a successful unlock")
	}
	if planning, err := sa.SessionIsPlanning(ctx, ref); err != nil || planning {
		t.Fatalf("session still planning after unlock (err=%v)", err)
	}
	// A second unlock is a no-op — still cleared.
	if err := phase.Unlock(); err != nil {
		t.Fatalf("idempotent unlock: %v", err)
	}

	// A failed write fails the unlock and the phase stays planning.
	db2 := testdb.New(t)
	ref2 := session.Direct(store.NewID())
	sa2 := store.NewEntryStoreFor(db2, ref2)
	if err := db2.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	phase2 := &middleware.PlanPhase{}
	armPlanUnlock(phase2, sa2, ref2)
	if err := phase2.Unlock(); err == nil {
		t.Fatal("a failed write must fail the unlock")
	}
	if phase2.Executing() {
		t.Fatal("the phase must stay locked when the write fails")
	}
}

// The read half: restorePlanPhase puts a run into the phase the SESSION's
// marker says — every run, not only a resume, so an approved plan is not
// re-asked next turn — replaying without duplicating.
func TestRestorePlanPhase(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:   store.NewAgentConfigStore(db),
		Settings:       settings.NewReader(store.NewSettingStore(db)),
		Memories:       store.NewMemoryStore(db),
		McpServers:     store.NewMcpServerStore(db),
		ProviderRoutes: store.NewProviderRouteStore(db),
		Guardrails:     NewGuardrailResolver(store.NewGuardrailStore(db)),
		SandboxManager: sandboxes.NewManager(t.TempDir()),
	})

	ref0, err := store.RefFor(ctx, db, sess.ID)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	sa := store.NewEntryStoreFor(db, ref0)
	sa.SetRunID("r1")

	// No marker: nobody asked for a plan, so the run executes. Plan mode is a
	// restraint, and who imposes it is the person, not the agent's build.
	fresh := &middleware.PlanPhase{}
	if err := runner.restorePlanPhase(ctx, fresh, sa, ref0); err != nil {
		t.Fatalf("restore (no marker): %v", err)
	}
	if !fresh.Executing() {
		t.Fatal("a session nobody asked a plan of must start executing")
	}

	// Asked for one: the locked marker puts the next run into planning, and the
	// armed hook persists the unlock when THIS run's plan is approved.
	if err := sa.SetSessionPlanning(ctx, ref0, true); err != nil {
		t.Fatalf("set planning: %v", err)
	}
	phase := &middleware.PlanPhase{}
	if err := runner.restorePlanPhase(ctx, phase, sa, ref0); err != nil {
		t.Fatalf("restore (asked to plan): %v", err)
	}
	if phase.Executing() {
		t.Fatal("a session asked for a plan must start in the planning phase")
	}
	if err := phase.Unlock(); err != nil {
		t.Fatalf("unlock after restore: %v", err)
	}

	// Unlocked: a LATER run — a different run id entirely — starts executing,
	// which is the whole point: the person approved once.
	phase2 := &middleware.PlanPhase{}
	sa2 := store.NewEntryStoreFor(db, ref0)
	sa2.SetRunID("r2")
	if err := runner.restorePlanPhase(ctx, phase2, sa2, ref0); err != nil {
		t.Fatalf("restore (already unlocked): %v", err)
	}
	if !phase2.Executing() {
		t.Fatal("an unlocked session must restore the executing phase")
	}
	if planning, err := sa.SessionIsPlanning(ctx, ref0); err != nil || planning {
		t.Fatalf("session must read as unlocked after the approved plan (err=%v)", err)
	}

	// A failed read surfaces as an error — never a silent planning phase.
	dbBroken := testdb.New(t)
	sessions2 := store.NewSessionStore(dbBroken)
	sess2 := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s2"}
	if err := sessions2.Create(ctx, sess2); err != nil {
		t.Fatalf("create session: %v", err)
	}
	broken := NewRunner(ctx, dbBroken, &AgentDeps{
		AgentConfigs:   store.NewAgentConfigStore(dbBroken),
		Settings:       settings.NewReader(store.NewSettingStore(dbBroken)),
		Memories:       store.NewMemoryStore(dbBroken),
		McpServers:     store.NewMcpServerStore(dbBroken),
		ProviderRoutes: store.NewProviderRouteStore(dbBroken),
		Guardrails:     NewGuardrailResolver(store.NewGuardrailStore(dbBroken)),
		SandboxManager: sandboxes.NewManager(t.TempDir()),
	})
	if err := dbBroken.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	brokenRef, err := store.RefFor(context.Background(), dbBroken, sess2.ID)
	if err != nil {
		brokenRef = session.Ref{ID: sess2.ID}
	}
	if err := broken.restorePlanPhase(ctx, &middleware.PlanPhase{}, store.NewEntryStoreFor(dbBroken, brokenRef), brokenRef); err == nil {
		t.Fatal("a failed marker read must abort the run, not fall back to planning")
	}
}

// A run request that is REFUSED (the session is busy) must not have changed the
// session's plan phase — the intent is applied inside the reservation, after
// the register that would refuse it, so a losing request leaves no trace.
func TestPlanIntentIsNotAppliedWhenTheRunIsRefused(t *testing.T) {
	ctx := context.Background()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	ac := &store.AgentConfig{Name: "a", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Occupy the session so the next start is refused as busy.
	if _, _, err := runner.hub.register("holder", sess.ID, "", ac.ID, "", "", nil); err != nil {
		t.Fatal(err)
	}

	plan := true
	if _, err := runner.StartRun(sess.ID, ac.ID, "", "", "hi", &plan, nil); err == nil {
		t.Fatal("a busy session must refuse the run")
	}
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	planning, err := store.NewEntryStoreFor(runner.db, ref).SessionIsPlanning(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if planning {
		t.Fatal("a refused plan:true request must not have entered plan mode")
	}
}
