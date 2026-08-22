package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func mkAgent(t *testing.T, s *store.AgentConfigStore, name string, handoffIDs ...string) string {
	t.Helper()
	ac := &store.AgentConfig{Name: name, Model: "gpt-test"}
	if len(handoffIDs) > 0 {
		raw, _ := json.Marshal(handoffIDs)
		ac.HandoffsJSON = string(raw)
	}
	if err := s.Create(context.Background(), ac); err != nil {
		t.Fatalf("create agent %s: %v", name, err)
	}
	return ac.ID
}

func handoffNames(built *BuildResult) []string {
	var names []string
	for _, h := range built.Agent.Handoffs {
		if h.Target != nil {
			names = append(names, h.Target.Name)
		}
	}
	return names
}

// A diamond handoff graph — A→B→D and A→C→D — is a legitimate DAG: D is a
// shared descendant, not a cycle. The build must keep BOTH edges to D; the old
// non-popped visited set flagged the second path to D as a cycle and silently
// dropped C→D.
func TestBuildFullAgentDiamondHandoffNotACycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{AgentConfigs: s, Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db)}

	d := mkAgent(t, s, "D")
	b := mkAgent(t, s, "B", d)
	c := mkAgent(t, s, "C", d)
	a := mkAgent(t, s, "A", b, c)

	built, err := BuildFullAgent(ctx, deps, a, "", store.LocalUserID)
	if err != nil {
		t.Fatalf("build A: %v", err)
	}
	if got := handoffNames(built); len(got) != 2 {
		t.Fatalf("A handoffs = %v, want [B C]", got)
	}
	// Both B and C must still hand off to D — the diamond's second edge must
	// survive. A shared node is built once and reused, so both point at the
	// same D instance.
	var bTarget, cTarget *BuildResult
	for _, h := range built.Agent.Handoffs {
		tgt := h.Target
		switch tgt.Name {
		case "B":
			bTarget = &BuildResult{Agent: tgt}
		case "C":
			cTarget = &BuildResult{Agent: tgt}
		}
	}
	if bTarget == nil || cTarget == nil {
		t.Fatal("missing B or C target")
	}
	if got := handoffNames(bTarget); len(got) != 1 || got[0] != "D" {
		t.Errorf("B handoffs = %v, want [D]", got)
	}
	if got := handoffNames(cTarget); len(got) != 1 || got[0] != "D" {
		t.Errorf("C handoffs = %v, want [D] — the diamond's second edge was dropped", got)
	}
}

// A handoff target with a genuinely broken config (not a cycle) must fail the
// parent build, not vanish silently — otherwise a misconfigured target hides a
// configured handoff.
func TestBuildFullAgentHandoffTargetErrorPropagates(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{
		AgentConfigs: s, Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db),
		Guardrails: NewGuardrailResolver(store.NewGuardrailStore(db)),
	}

	// Target B has a broken output_schema; A hands off to B.
	bad := &store.AgentConfig{Name: "B", Model: "gpt-test", Guardrails: store.GuardrailGroup{OutputSchema: "{not json"}}
	if err := s.Create(ctx, bad); err != nil {
		t.Fatalf("create B: %v", err)
	}
	a := mkAgent(t, s, "A", bad.ID)

	_, err := BuildFullAgent(ctx, deps, a, "", store.LocalUserID)
	if err == nil || !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("a broken handoff target must fail the build, got %v", err)
	}
}

// A real cycle (A→B→A) must still be broken: A builds, B builds, and B's
// back-edge to A is skipped rather than recursing forever.
func TestBuildFullAgentRealCycleBroken(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{AgentConfigs: s, Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db)}

	// Create A and B, then point B back at A to form the cycle.
	a := mkAgent(t, s, "A")
	b := mkAgent(t, s, "B", a)
	aCfg, _ := s.Get(ctx, a)
	raw, _ := json.Marshal([]string{b})
	aCfg.HandoffsJSON = string(raw)
	if err := s.Update(ctx, a, aCfg, nil); err != nil {
		t.Fatalf("update A: %v", err)
	}

	built, err := BuildFullAgent(ctx, deps, a, "", store.LocalUserID)
	if err != nil {
		t.Fatalf("build A (cycle must be broken, not error): %v", err)
	}
	// A→B kept; B→A is the back-edge and is dropped.
	if got := handoffNames(built); len(got) != 1 || got[0] != "B" {
		t.Fatalf("A handoffs = %v, want [B]", got)
	}
	for _, h := range built.Agent.Handoffs {
		tgt := h.Target
		if tgt.Name == "B" {
			if got := handoffNames(&BuildResult{Agent: tgt}); len(got) != 0 {
				t.Errorf("B handoffs = %v, want [] (back-edge to A dropped)", got)
			}
		}
	}
}
