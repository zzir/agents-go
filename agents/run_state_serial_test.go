package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRunState_RoundTripCarriesPendingInputDisclosedToolsCursor pins the rule
// that the serialized surface covers every field a resume consumes. Pending
// injected input, disclosed deferred tools and the server cursor used to ride
// along only in the live struct: an in-process resume kept them, a
// cross-process one silently dropped them.
func TestRunState_RoundTripCarriesPendingInputDisclosedToolsCursor(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := res.State
	if state == nil {
		t.Fatal("expected a paused state")
	}
	state.PendingInput = PendingInput{
		Steer:    InputItemsFromText("steer this way"),
		NextTurn: InputItemsFromText("then this"),
		FollowUp: InputItemsFromText("and follow up"),
	}
	state.DisclosedTools = []string{"deferred_a", "deferred_b"}
	state.cursor = serverCursor{responseID: "resp_42", itemCount: 3, conversationActive: true}

	data, err := state.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.DisclosedTools; len(got) != 2 || got[0] != "deferred_a" || got[1] != "deferred_b" {
		t.Errorf("DisclosedTools = %v", got)
	}
	if restored.cursor != state.cursor {
		t.Errorf("cursor = %+v, want %+v", restored.cursor, state.cursor)
	}
	queues := []struct {
		name      string
		want, got []InputItem
	}{
		{"Steer", state.PendingInput.Steer, restored.PendingInput.Steer},
		{"NextTurn", state.PendingInput.NextTurn, restored.PendingInput.NextTurn},
		{"FollowUp", state.PendingInput.FollowUp, restored.PendingInput.FollowUp},
	}
	for _, q := range queues {
		want, _ := json.Marshal(q.want)
		got, _ := json.Marshal(q.got)
		if !bytes.Equal(want, got) {
			t.Errorf("PendingInput.%s = %s, want %s", q.name, got, want)
		}
	}
}

// TestHITL_ResumeSendsDeltaWithRestoredCursor locks the server-cursor half of
// the same rule, end to end in previous_response_id mode. The pause happens
// mid-execution — a sibling tool completed before a nested agent-as-tool run
// paused for approval — so the pre-pause tool output sits past the cursor,
// still pending. After a cross-process resume the post-approval model call
// must chain to the paused response and send exactly the pending outputs; a
// re-derived cursor used to mark the sibling's output as already served, and
// the server never received it.
func TestHITL_ResumeSendsDeltaWithRestoredCursor(t *testing.T) {
	var autoRan, gatedRan bool
	auto := NewTool("auto_tool", "runs without approval",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			autoRan = true
			return "auto done", nil
		})
	innerTool := NewTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			gatedRan = true
			return "deleted", nil
		})
	innerTool.NeedsApproval = true
	innerModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "inner_call", `{}`)),
		modelResp(messageOutput(t, "inner finished")),
	}}
	inner := &Agent{Name: "specialist", Tools: []*Tool{innerTool}, ModelImpl: innerModel}

	outerModel := &fakeModel{responses: []*ModelResponse{
		{
			ResponseID: "resp_1",
			Output: []OutputItem{
				functionCallOutput(t, "auto_tool", "call_auto", `{}`),
				functionCallOutput(t, "specialist", "call_spec", `{"input":"go"}`),
			},
			Usage: &Usage{Requests: 1, TotalTokens: 1},
		},
		modelResp(messageOutput(t, "outer finished")),
	}}
	outer := &Agent{
		Name:      "triage",
		Tools:     []*Tool{auto, inner.AsTool(AgentToolConfig{Name: "specialist", Description: "delegate"})},
		ModelImpl: outerModel,
	}
	opts := RunOptions{Conversation: ConversationOptions{UsePreviousResponseID: true}}

	res, err := RunSync(context.Background(), outer, "run both", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 || !autoRan || gatedRan {
		t.Fatalf("pause shape wrong: interruptions=%d autoRan=%v gatedRan=%v",
			len(res.Interruptions), autoRan, gatedRan)
	}

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	registry := map[string]*Agent{"triage": outer, "specialist": inner}
	state, err := RunStateFromJSON(data, registry)
	if err != nil {
		t.Fatal(err)
	}
	state.Approve(state.Interruptions[0], false)
	if _, err := ResumeRunSync(context.Background(), state, opts); err != nil {
		t.Fatal(err)
	}
	if !gatedRan {
		t.Fatal("approved nested tool did not run")
	}

	req := outerModel.lastReq
	if req.PreviousResponseID != "resp_1" {
		t.Errorf("PreviousResponseID = %q, want resp_1 (cursor lost across resume)", req.PreviousResponseID)
	}
	if len(req.Input) != 2 {
		t.Fatalf("delta has %d items, want 2 tool outputs (the pre-pause sibling output must not be skipped)", len(req.Input))
	}
	for i, it := range req.Input {
		raw, _ := json.Marshal(it)
		if !strings.Contains(string(raw), "function_call_output") {
			t.Errorf("delta[%d] = %s, want a function_call_output", i, raw)
		}
	}
}
