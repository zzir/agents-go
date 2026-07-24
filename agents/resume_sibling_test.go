package agents

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestResume_SiblingToolNotReExecutedAfterNestedApproval covers audit: a turn
// runs a plain sibling tool S alongside a nested agent-as-tool A whose nested run
// pauses for approval. When the interrupted response is re-processed on resume, S
// reappears as a pending call. Resuming must NOT re-run S (its side effect fired
// once, and its output was recorded before the pause) and must not emit a second
// function_call_output for S (a duplicate call id the Responses API rejects).
func TestResume_SiblingToolNotReExecutedAfterNestedApproval(t *testing.T) {
	var siblingRuns atomic.Int32
	siblingTool := NewFunctionTool("sibling", "harmless",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			siblingRuns.Add(1)
			return "sibling-done", nil
		})

	var innerRan bool
	innerTool := NewFunctionTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			innerRan = true
			return "deleted", nil
		})
	innerTool.NeedsApproval = true
	inner := &Agent{Name: "specialist", Tools: []Tool{innerTool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "inner_call", `{}`)),
		modelResp(messageOutput(t, "inner finished")),
	}}}

	outer := &Agent{
		Name: "triage",
		Tools: []Tool{
			siblingTool,
			inner.AsTool(AgentToolConfig{Name: "specialist", Description: "delegate"}),
		},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			// A single turn emitting two tool calls: the plain sibling and the
			// nested agent tool (which pauses inside its own run).
			modelResp(
				functionCallOutput(t, "sibling", "sibling_call", `{}`),
				functionCallOutput(t, "specialist", "outer_call", `{"input":"go"}`),
			),
			modelResp(messageOutput(t, "outer finished")),
		}},
	}

	res, err := Run(context.Background(), outer, "handle it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want 1 (nested delete_db approval surfaced)", len(res.Interruptions))
	}
	if got := siblingRuns.Load(); got != 1 {
		t.Fatalf("sibling ran %d times before resume, want 1", got)
	}
	if n := countToolOutputs(res.NewItems, "sibling_call"); n != 1 {
		t.Fatalf("sibling outputs before resume = %d, want 1", n)
	}

	// Approve the surfaced nested interruption and resume the parent run.
	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !innerRan {
		t.Error("nested delete_db did not run after approval")
	}
	if len(res2.Interruptions) != 0 {
		t.Errorf("still interrupted after approval: %d", len(res2.Interruptions))
	}
	if got := siblingRuns.Load(); got != 1 {
		t.Errorf("sibling re-executed on resume: ran %d times total, want 1", got)
	}
	// Exactly one function_call_output for the sibling call may exist in the
	// final item log; a second would 400 at the Responses API on the next turn.
	if n := countToolOutputs(res2.NewItems, "sibling_call"); n != 1 {
		t.Errorf("sibling outputs in final items = %d, want 1", n)
	}
	if res2.FinalOutputString() != "outer finished" {
		t.Errorf("final = %q, want %q", res2.FinalOutputString(), "outer finished")
	}
}

// countToolOutputs counts function_call_output items whose call id matches.
func countToolOutputs(items []RunItem, callID string) int {
	n := 0
	for _, it := range items {
		if id, _, isOutput := runItemCallID(it); isOutput && id == callID {
			n++
		}
	}
	return n
}

// TestSafePersistBoundary covers audit: the persist boundary must never leave
// a stored function_call without its matching function_call_output, even when a
// completed sibling's output is ordered after a still-pending (interrupted) call.
func TestSafePersistBoundary(t *testing.T) {
	call := func(callID string) RunItem { return &ToolCallItem{Raw: functionCallOutput(t, "t", callID, `{}`)} }
	out := func(callID string) RunItem { return newFunctionCallOutputItem(nil, callID, "out") }
	msg := func() RunItem { return &MessageOutputItem{Raw: messageOutput(t, "hi")} }

	cases := []struct {
		name  string
		items []RunItem
		start int
		want  int
	}{
		{
			// audit: sibling S's output sits AFTER the pending call A, so S's
			// own call must be held back too — the stored history must never keep
			// S's call without S's output.
			name:  "completed sibling output after interrupted call",
			items: []RunItem{call("S"), call("A"), out("S")},
			want:  0,
		},
		{
			name:  "all calls paired persists in full",
			items: []RunItem{call("S"), out("S"), call("T"), out("T")},
			want:  4,
		},
		{
			name:  "trailing interrupted call held back",
			items: []RunItem{call("S"), out("S"), call("A")},
			want:  2,
		},
		{
			name:  "message-only persists",
			items: []RunItem{msg()},
			want:  1,
		},
		{
			name:  "start past end",
			items: []RunItem{call("S")},
			start: 1,
			want:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safePersistBoundary(tc.items, tc.start)
			if got != tc.want {
				t.Errorf("safePersistBoundary = %d, want %d", got, tc.want)
			}
			// Invariant: the persisted prefix never contains a dangling call.
			assertNoDanglingCall(t, tc.items[tc.start:got])
		})
	}
}

func assertNoDanglingCall(t *testing.T, items []RunItem) {
	t.Helper()
	haveOutput := map[string]bool{}
	for _, it := range items {
		if id, _, isOutput := runItemCallID(it); isOutput {
			haveOutput[id] = true
		}
	}
	for _, it := range items {
		if id, isCall, _ := runItemCallID(it); isCall && !haveOutput[id] {
			t.Errorf("persisted prefix contains dangling call %q (no output)", id)
		}
	}
}
