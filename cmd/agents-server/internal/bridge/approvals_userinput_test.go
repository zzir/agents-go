package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func userInputItems(t *testing.T, raws ...string) []agents.InputItem {
	t.Helper()
	// Decode per item via the SDK so role-only messages ({"role":"user",...})
	// resolve to the EasyInputMessage variant, exactly as a real run's input does.
	items := make([]agents.InputItem, 0, len(raws))
	for _, r := range raws {
		it, err := session.UnmarshalInputItem([]byte(r))
		if err != nil {
			t.Fatalf("unmarshal input item %q: %v", r, err)
		}
		items = append(items, it)
	}
	return items
}

// pausedState is the SDK state of a run paused on one shell call.
func pausedState(t *testing.T) *agents.RunState {
	t.Helper()
	var rawCall agents.OutputItem
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call-1","name":"shell","arguments":"{}"}`), &rawCall); err != nil {
		t.Fatalf("unmarshal raw call: %v", err)
	}
	return &agents.RunState{
		CurrentAgent: &agents.Agent{Name: "a"},
		Approvals:    agents.NewApprovalStore(),
		UserInput:    userInputItems(t, `{"role":"user","content":"remove prod"}`),
		Interruptions: []*agents.ToolApprovalItem{{
			Agent:    &agents.Agent{Name: "a"},
			ToolName: "shell",
			CallID:   "call-1",
			Raw:      rawCall,
		}},
	}
}

// persistInterruption keeps the paused turn's user prompt on the row, so a
// reload during approval rebuilds the bubble without the entries.
func TestPersistInterruptionStoresUserInput(t *testing.T) {
	runID := store.NewID()
	ctx := context.Background()
	db := testdb.New(t)
	approvals := store.NewPendingApprovalStore(db)
	runner := testRunner(ctx, t, db)

	result := &RunOutcome{
		RunID:       runID,
		SessionID:   store.NewID(),
		Interrupted: true,
		SDKState:    pausedState(t),
		Interruptions: []*agents.ToolApprovalItem{
			{ToolName: "shell", CallID: "call-1"},
		},
	}

	if err := runner.persistInterruption(result); err != nil {
		t.Fatalf("persistInterruption: %v", err)
	}
	got, err := approvals.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if got.UserInput != "remove prod" {
		t.Errorf("stored UserInput = %q, want %q", got.UserInput, "remove prod")
	}
}

// A pause is durable or it is an error, never a silent skip: no Runner exists
// without the approval store, a pause with nothing to resume from is refused,
// and a failed write fails the segment before the pause is announced.
func TestAPauseThatCannotBePersistedIsNeverAnnounced(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	deps := testDeps(t, db)
	deps.PendingApprovals = nil
	if _, err := NewRunner(ctx, db, deps); err == nil || !strings.Contains(err.Error(), "PendingApprovals") {
		t.Fatalf("NewRunner without the approval store: err = %v, want a refusal naming it", err)
	}

	runner := testRunner(ctx, t, db)
	paused := func() *agents.RunResult {
		state := pausedState(t)
		return &agents.RunResult{Interruptions: state.Interruptions, State: state}
	}
	var announced []string
	send := func(typ string, _ any) { announced = append(announced, typ) }
	finish := func(res *agents.RunResult) error {
		_, err := runner.finishResult(res, store.NewID(), store.NewID(), "", "", send)
		return err
	}

	stateless := paused()
	stateless.State = nil
	if err := finish(stateless); err == nil {
		t.Error("a pause with no state to resume from was accepted")
	}
	// The store refuses the write: the database is gone.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := finish(paused()); err == nil {
		t.Error("a pause whose row could not be written was accepted")
	}
	if len(announced) != 0 {
		t.Errorf("events sent for a pause that was never recorded: %v", announced)
	}
}
