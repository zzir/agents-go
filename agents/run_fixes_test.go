package agents

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"
)

// Resuming must not re-append the interrupted response's own items: the
// function_call would appear twice in the next model input and the API would
// reject the dangling duplicate.
func TestHITL_ResumeDoesNotDuplicateItems(t *testing.T) {
	var ran bool
	agent, model := approvalAgentAndModel(t, &ran)

	res, err := Run(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if n := countToolCalls(t, res2.NewItems); n != 1 {
		t.Errorf("tool_call items = %d, want 1 (no duplicates)", n)
	}
	// The final model input must contain the call exactly once.
	b, _ := json.Marshal(model.lastReq.Input)
	if n := strings.Count(string(b), `"function_call"`); n != 1 {
		t.Errorf("final model input has %d function_call items, want 1: %s", n, b)
	}
}

func countToolCalls(t *testing.T, items []RunItem) int {
	t.Helper()
	n := 0
	for _, it := range items {
		if it.ItemType() == "tool_call" {
			n++
		}
	}
	return n
}

// The user input that triggered an interruption must reach the session once
// the resumed run completes — including across a serialize/restore cycle.
func TestHITL_SessionSavesUserInputAcrossResume(t *testing.T) {
	var ran bool
	agent, _ := approvalAgentAndModel(t, &ran)
	session := NewInMemorySession()

	res, err := Run(context.Background(), agent, "delete it", RunOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := session.GetItems(context.Background(), 0); len(got) != 0 {
		t.Fatalf("interrupted run should not have saved items yet, got %d", len(got))
	}

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	state, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	state.Approve(state.Interruptions[0], false)
	if _, err := ResumeRun(context.Background(), state, RunOptions{Session: session}); err != nil {
		t.Fatal(err)
	}

	items, err := session.GetItems(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(items)
	s := string(b)
	if !strings.Contains(s, "delete it") {
		t.Errorf("session lost the user input: %s", s)
	}
	if n := strings.Count(s, `"function_call"`); n != 1 {
		t.Errorf("session has %d function_call items, want 1: %s", n, s)
	}
	if !strings.Contains(s, "all done") {
		t.Errorf("session lost the final assistant message: %s", s)
	}
}

// When the model requests several handoffs in one turn, only the first is
// taken but every other call still needs a tool output, or the next model
// call is rejected for a dangling function_call.
func TestRun_MultipleHandoffsGetRejectionOutputs(t *testing.T) {
	target1 := &Agent{Name: "t1", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "from t1"))}}}
	target2 := &Agent{Name: "t2", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "from t2"))}}}
	src := &Agent{
		Name: "src",
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(
				functionCallOutput(t, "transfer_to_t1", "call_1", `{}`),
				functionCallOutput(t, "transfer_to_t2", "call_2", `{}`),
			),
		}},
		Handoffs: []Handoff{HandoffTo(target1), HandoffTo(target2)},
	}

	res, err := Run(context.Background(), src, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.LastAgent != target1 {
		t.Errorf("last agent = %q, want t1", res.LastAgent.Name)
	}
	var rejected bool
	for _, it := range res.NewItems {
		if o, ok := it.(*ToolCallOutputItem); ok && o.Output == multipleHandoffsMessage {
			rejected = true
		}
	}
	if !rejected {
		t.Error("expected a rejection output for the ignored handoff")
	}
	// Every function_call in the conversation must have a matching output.
	in, err := itemsToInputList(res.NewItems)
	if err != nil {
		t.Fatal(err)
	}
	calls, outputs := map[string]bool{}, map[string]bool{}
	for _, item := range in {
		if item.OfFunctionCall != nil {
			calls[item.OfFunctionCall.CallID] = true
		}
		if item.OfFunctionCallOutput != nil {
			outputs[item.OfFunctionCallOutput.CallID] = true
		}
	}
	for id := range calls {
		if !outputs[id] {
			t.Errorf("dangling function_call %q without output", id)
		}
	}
}

// A handoff input filter rewrites what the next agent sees, but must not
// affect what is saved to the session (per the InputFilter contract).
func TestRun_HandoffFilterDoesNotAffectSession(t *testing.T) {
	target := &Agent{Name: "target", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "target done"))}}}
	h := HandoffTo(target)
	h.InputFilter = func(d HandoffInputData) HandoffInputData {
		// Drop everything but the last item for the next agent.
		if len(d.InputHistory) > 1 {
			d.InputHistory = d.InputHistory[len(d.InputHistory)-1:]
		}
		return d
	}
	src := &Agent{
		Name:      "src",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_target", "call_1", `{}`))}},
		Handoffs:  []Handoff{h},
	}
	session := NewInMemorySession()
	if _, err := Run(context.Background(), src, "hi", RunOptions{Session: session}); err != nil {
		t.Fatal(err)
	}
	items, _ := session.GetItems(context.Background(), 0)
	b, _ := json.Marshal(items)
	s := string(b)
	if !strings.Contains(s, "transfer_to_target") {
		t.Errorf("session lost pre-handoff items after input filter: %s", s)
	}
	if !strings.Contains(s, "target done") {
		t.Errorf("session lost post-handoff items: %s", s)
	}
}

// A streamed run that pauses for approval must return a resumable state.
func TestRunStreamed_InterruptionReturnsState(t *testing.T) {
	var ran bool
	agent, _ := approvalAgentAndModel(t, &ran)

	sr := RunStreamed(context.Background(), agent, "delete it", RunOptions{})
	for _, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := sr.FinalResult()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want 1", len(res.Interruptions))
	}
	if res.State == nil {
		t.Fatal("streamed interruption must carry a resumable RunState")
	}

	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("tool should have run after approval")
	}
	if res2.FinalOutputString() != "all done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}

// silentStreamModel streams no events at all (e.g. the stream failed before a
// completed event): the run must error rather than succeed with empty output.
type silentStreamModel struct{}

func (silentStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{Usage: NewUsage()}, nil
}

func (silentStreamModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {}
}

// After an agent calls a tool, tool_choice must be left unset on later turns
// (unless disabled) so ToolChoiceRequired cannot force an infinite loop.
func TestRun_ToolChoiceResetAfterToolUse(t *testing.T) {
	newAgent := func(disable bool) (*Agent, *fakeModel) {
		tool := NewFunctionTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
			return "ok", nil
		})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "noop", "c1", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}
		return &Agent{
			Name:                   "a",
			Tools:                  []Tool{tool},
			ModelImpl:              model,
			ModelSettings:          &ModelSettings{ToolChoice: ToolChoiceRequired},
			DisableToolChoiceReset: disable,
		}, model
	}

	agent, model := newAgent(false)
	if _, err := Run(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := model.lastReq.Settings.ToolChoice; got != "" {
		t.Errorf("turn-2 tool_choice = %q, want unset after tool use", got)
	}

	agent, model = newAgent(true)
	if _, err := Run(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := model.lastReq.Settings.ToolChoice; got != ToolChoiceRequired {
		t.Errorf("turn-2 tool_choice = %q, want %q with reset disabled", got, ToolChoiceRequired)
	}
}

func TestRunStreamed_NoCompletedEventIsAnError(t *testing.T) {
	agent := &Agent{Name: "a", ModelImpl: silentStreamModel{}}
	sr := RunStreamed(context.Background(), agent, "hi", RunOptions{})
	var streamErr error
	for _, err := range sr.Events() {
		if err != nil {
			streamErr = err
		}
	}
	if streamErr == nil {
		t.Fatal("expected a stream error when no completed event arrives")
	}
	if _, err := sr.FinalResult(); err == nil {
		t.Fatal("expected FinalResult error when no completed event arrives")
	}
}
