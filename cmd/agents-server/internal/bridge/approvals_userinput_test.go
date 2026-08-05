package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
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

func TestUserInputText(t *testing.T) {
	cases := []struct {
		name string
		raws []string
		want string
	}{
		{
			name: "plain string content",
			raws: []string{`{"role":"user","content":"delete the database"}`},
			want: "delete the database",
		},
		{
			name: "content array parts",
			raws: []string{`{"role":"user","content":[{"type":"input_text","text":"run the deploy"}]}`},
			want: "run the deploy",
		},
		{
			name: "skips non-user items",
			raws: []string{
				`{"role":"system","content":"be careful"}`,
				`{"role":"user","content":"go"}`,
			},
			want: "go",
		},
		{
			name: "empty when no user item",
			raws: []string{`{"role":"assistant","content":"hi"}`},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := userInputText(userInputItems(t, tc.raws...))
			if got != tc.want {
				t.Errorf("userInputText = %q, want %q", got, tc.want)
			}
		})
	}
}

// persistInterruption must capture the paused turn's user prompt so a reload
// during approval can rebuild the user bubble (the SDK only writes the turn to
// `messages` on completion).
func TestPersistInterruptionStoresUserInput(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	approvals := store.NewPendingApprovalStore(db)
	runner := NewRunner(ctx, db, &AgentDeps{PendingApprovals: approvals})

	var rawCall agents.OutputItem
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call-1","name":"shell","arguments":"{}"}`), &rawCall); err != nil {
		t.Fatalf("unmarshal raw call: %v", err)
	}
	state := &agents.RunState{
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
	result := &RunResult{
		RunID:       "run-1",
		SessionID:   "sess-1",
		Interrupted: true,
		SDKState:    state,
		Interruptions: []*agents.ToolApprovalItem{
			{ToolName: "shell", CallID: "call-1"},
		},
	}

	if err := runner.persistInterruption(result); err != nil {
		t.Fatalf("persistInterruption: %v", err)
	}
	got, err := approvals.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if got.UserInput != "remove prod" {
		t.Errorf("stored UserInput = %q, want %q", got.UserInput, "remove prod")
	}
}
