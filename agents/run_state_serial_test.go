package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	state.OffChainHistory = true

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
	// Lose this one across processes and a chain-based compaction on the resumed
	// run summarizes away what the paused half never sent — deleted unread.
	if !restored.OffChainHistory {
		t.Error("OffChainHistory = false; the paused run's log held items no model call carried")
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

// Approval id lists serialize sorted, so two otherwise identical runs produce
// identical bytes. Straight off the map they came out in iteration order, and a
// caller diffing persisted states saw churn that meant nothing.
func TestRunState_ApprovalIDsSerializeSorted(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c3", "c1", "c2"} {
		res.State.Approve(&ToolApprovalItem{ToolName: "delete_db", CallID: id}, false)
	}
	for _, id := range []string{"r2", "r1"} {
		res.State.Reject(&ToolApprovalItem{ToolName: "delete_db", CallID: id}, false, "no")
	}

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	again, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Errorf("two marshals of one state differ:\n%s\n%s", data, again)
	}

	var decoded serialRunState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	entry := decoded.ApprovalEntries["delete_db"]
	if want := []string{"c1", "c2", "c3"}; !slices.Equal(entry.ApprovedIDs, want) {
		t.Errorf("ApprovedIDs = %v, want %v", entry.ApprovedIDs, want)
	}
	if want := []string{"r1", "r2"}; !slices.Equal(entry.RejectedIDs, want) {
		t.Errorf("RejectedIDs = %v, want %v", entry.RejectedIDs, want)
	}
}

// The decode gate: same major, no newer than this SDK, no older than the oldest
// minor whose fields still mean what they say. The window is written in terms
// of the constants rather than literals, so it keeps testing the boundary
// wherever the next bump moves it — including the case the boundary exists for,
// a minor just below the floor whose fields the decoder would silently drop.
func TestRunStateFromJSON_SchemaVersionAcceptance(t *testing.T) {
	t.Parallel()
	registry := map[string]*Agent{"a": {Name: "a"}}
	major, minor, ok := parseSchemaVersion(RunStateSchemaVersion)
	if !ok {
		t.Fatalf("RunStateSchemaVersion = %q, want major.minor", RunStateSchemaVersion)
	}
	sameMajor := func(minor int) string { return fmt.Sprintf("%d.%d", major, minor) }
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"current", RunStateSchemaVersion, false},
		{"oldest decodable minor", sameMajor(runStateOldestDecodableMinor), false},
		{"below the oldest decodable minor", sameMajor(runStateOldestDecodableMinor - 1), true},
		{"newer minor", sameMajor(minor + 1), true},
		{"other major", fmt.Sprintf("%d.0", major+1), true},
		{"absent", "", true},
		{"not a number", "1.x", true},
		{"three parts", RunStateSchemaVersion + ".1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunStateFromJSON(minimalStateJSON(tc.version), registry)
			if tc.wantErr {
				var ue *UserError
				if !errors.As(err, &ue) {
					t.Fatalf("err = %v (%T), want a *UserError", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decoding version %q: %v", tc.version, err)
			}
		})
	}
}

// 1.3 stays outside the window whatever else moves. Tags v0.1.0 and v0.1.1
// stamped it while the guardrail results were four separate keys, v0.2.0 and
// v0.2.1 stamped it after they collapsed into today's guardrail_results, and
// nothing in the string tells the two apart — so accepting 1.3 would decode a
// v0.1.x state successfully with every guardrail result dropped, and resume is
// the one path that carries first-turn input-guardrail results forward.
// Widening the window is fine; widening it back over 1.3 is not.
func TestRunStateFromJSON_RejectsAmbiguousReleasedMinor(t *testing.T) {
	t.Parallel()
	_, err := RunStateFromJSON(minimalStateJSON("1.3"), map[string]*Agent{"a": {Name: "a"}})
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want a *UserError refusing 1.3", err, err)
	}
}

// A state carrying none of the fields later minors added still resumes, reading
// them through the fallbacks written for them all along. Those fallbacks are
// what the version window spends: without them an older minor could not be
// accepted at all, and a run paused for a human would not survive the SDK
// upgrade that happened while they were deciding.
func TestRunStateFromJSON_AbsentFieldsFallBackAndResume(t *testing.T) {
	agent := &Agent{
		Name:      "a",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "carried on"))}},
	}
	st, err := RunStateFromJSON(minimalStateJSON(RunStateSchemaVersion), map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	// An absent field falls back rather than decoding as its zero value, where
	// zero would mean something else.
	if !st.usagePending {
		t.Error("usagePending = false, want the pre-flag default (re-arm attribution)")
	}
	if !st.PendingInput.Empty() {
		t.Errorf("PendingInput = %+v, want empty", st.PendingInput)
	}
	if st.DisclosedTools != nil {
		t.Errorf("DisclosedTools = %v, want nil", st.DisclosedTools)
	}
	if st.cursor != (serverCursor{}) {
		t.Errorf("cursor = %+v, want zero", st.cursor)
	}

	res, err := ResumeRunSync(context.Background(), st, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutput != "carried on" {
		t.Errorf("FinalOutput = %q, want %q", res.FinalOutput, "carried on")
	}
}

// minimalStateJSON is a serialized state carrying only what the decoder needs
// to resolve an agent, stamped with the given version. Everything a later minor
// added is absent, so decoding it exercises the fallbacks.
func minimalStateJSON(version string) []byte {
	return []byte(`{"schema_version":"` + version + `","current_agent":"a","current_turn":1,` +
		`"original_input":[],"generated_items":[],"model_responses":[],` +
		`"interrupted_response":null,"interruptions":[]}`)
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
