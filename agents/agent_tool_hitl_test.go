package agents

import (
	"context"
	"encoding/json"
	"testing"
)

// nestedApprovalSetup builds an outer agent that calls an inner agent (as a
// tool) whose only tool requires approval. ran reports whether the inner
// approval-gated tool executed.
func nestedApprovalSetup(t *testing.T, ran *bool) *Agent {
	t.Helper()

	innerTool := NewFunctionTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			*ran = true
			return "deleted", nil
		})
	innerTool.NeedsApproval = true
	innerModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "inner_call", `{}`)),
		modelResp(messageOutput(t, "inner finished")),
	}}
	inner := &Agent{Name: "specialist", Tools: []*FunctionTool{innerTool}, ModelImpl: innerModel}

	outerModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "specialist", "outer_call", `{"input":"go"}`)),
		modelResp(messageOutput(t, "outer finished")),
	}}
	return &Agent{
		Name:      "triage",
		Tools:     []*FunctionTool{inner.AsTool(AgentToolConfig{Name: "specialist", Description: "delegate"})},
		ModelImpl: outerModel,
	}
}

func TestAgentTool_NestedApprovalSurfacesToParent(t *testing.T) {
	var ran bool
	outer := nestedApprovalSetup(t, &ran)

	res, err := RunSync(context.Background(), outer, "handle it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The nested approval surfaces as the parent run's interruption.
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want 1 (nested approval surfaced)", len(res.Interruptions))
	}
	if res.Interruptions[0].ToolName != "delete_db" {
		t.Errorf("interruption tool = %q, want delete_db (the nested tool)", res.Interruptions[0].ToolName)
	}
	if ran {
		t.Error("nested tool ran before approval")
	}
	if res.State == nil {
		t.Fatal("expected RunState for the paused parent run")
	}
}

func TestAgentTool_NestedApproveResumes(t *testing.T) {
	var ran bool
	outer := nestedApprovalSetup(t, &ran)

	res, err := RunSync(context.Background(), outer, "handle it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want 1", len(res.Interruptions))
	}

	// Approve the surfaced nested interruption and resume the parent run.
	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("nested tool did not run after approval (nested run was not resumed)")
	}
	if len(res2.Interruptions) != 0 {
		t.Errorf("still interrupted after approval: %d", len(res2.Interruptions))
	}
	if res2.FinalOutputString() != "outer finished" {
		t.Errorf("final = %q, want %q", res2.FinalOutputString(), "outer finished")
	}
}

func TestAgentTool_NestedRejectResumes(t *testing.T) {
	var ran bool
	outer := nestedApprovalSetup(t, &ran)

	res, err := RunSync(context.Background(), outer, "handle it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want 1", len(res.Interruptions))
	}

	// Reject the nested interruption: the parent run still completes, the nested
	// tool never runs, and the inner agent recovers from the rejection message.
	res.State.Reject(res.Interruptions[0], false, "denied by policy")
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("nested tool ran despite rejection")
	}
	if res2.FinalOutputString() != "outer finished" {
		t.Errorf("final = %q, want %q", res2.FinalOutputString(), "outer finished")
	}
}

// A completed agent-as-tool nested run folds its usage into the parent
// run's usage (Python parity: the nested run shares the parent's usage).
func TestAgentTool_NestedUsageAccumulatesIntoParent(t *testing.T) {
	inner := &Agent{
		Name:      "specialist",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "inner done"))}},
	}
	outer := &Agent{
		Name:  "triage",
		Tools: []*FunctionTool{inner.AsTool(AgentToolConfig{Name: "specialist", Description: "delegate"})},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "specialist", "c1", `{"input":"go"}`)),
			modelResp(messageOutput(t, "outer done")),
		}},
	}

	res, err := RunSync(context.Background(), outer, "handle it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Outer made 2 model calls, inner made 1 — all three requests must be
	// counted in the parent's usage.
	if res.Usage.Requests != 3 {
		t.Errorf("Usage.Requests = %d, want 3 (2 outer + 1 nested)", res.Usage.Requests)
	}
	if res.Usage.TotalTokens != 24 {
		t.Errorf("Usage.TotalTokens = %d, want 24 (3 calls * 8)", res.Usage.TotalTokens)
	}
}

// A parent run paused on a nested agent-as-tool approval survives a JSON
// round-trip: the paused nested state is serialized recursively, so a resume
// rebuilt with a registry containing the sub-agent CONTINUES the nested run
// (delete_db executes) instead of restarting it. Before schema 1.2 the nested
// state was dropped, so the resume restarted the nested run and never ran the
// gated tool.
func TestAgentTool_NestedStateSerializationRoundTrip(t *testing.T) {
	var ran bool
	innerTool := NewFunctionTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ran = true
			return "deleted", nil
		})
	innerTool.NeedsApproval = true
	inner := &Agent{Name: "specialist", Tools: []*FunctionTool{innerTool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "inner_call", `{}`)),
		modelResp(messageOutput(t, "inner finished")),
	}}}
	outer := &Agent{
		Name:  "triage",
		Tools: []*FunctionTool{inner.AsTool(AgentToolConfig{Name: "specialist", Description: "delegate"})},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "specialist", "outer_call", `{"input":"go"}`)),
			modelResp(messageOutput(t, "outer finished")),
		}},
	}

	res, err := RunSync(context.Background(), outer, "handle it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 || res.State == nil {
		t.Fatalf("expected 1 interruption + state, got %d interruptions", len(res.Interruptions))
	}

	// Round-trip the paused parent state through JSON and rebuild it with a
	// registry containing BOTH agents (the nested CurrentAgent resolves via the
	// same registry).
	data, err := json.Marshal(res.State)
	if err != nil {
		t.Fatal(err)
	}
	registry := map[string]*Agent{"triage": outer, "specialist": inner}
	rebuilt, err := RunStateFromJSON(data, registry)
	if err != nil {
		t.Fatal(err)
	}

	// Approve the surfaced nested interruption on the rebuilt state and resume.
	rebuilt.Approve(rebuilt.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), rebuilt, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("nested tool did not run after cross-process resume (nested state was not serialized)")
	}
	if len(res2.Interruptions) != 0 {
		t.Errorf("still interrupted after approval: %d", len(res2.Interruptions))
	}
	if res2.FinalOutputString() != "outer finished" {
		t.Errorf("final = %q, want %q", res2.FinalOutputString(), "outer finished")
	}
}
