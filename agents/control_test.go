package agents

import (
	"context"
	"strings"
	"testing"
)

// inputTexts renders a model request's input for assertions.
func inputTexts(items []TResponseInputItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(inputItemText(it))
		b.WriteString("|")
	}
	return b.String()
}

// Steer reaches the model whether or not the agent thought it was finished.
func TestSteer_ForcesAnotherTurn(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "all done")),
		modelResp(messageOutput(t, "revised")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	steered := false
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if m, ok := ev.(*RunItemStreamEvent); ok && !steered {
			if _, isMsg := m.Item.(*MessageOutputItem); isMsg {
				// The agent just answered; change course before it can finish.
				if err := ctrl.Steer("actually, do it differently"); err != nil {
					t.Fatal(err)
				}
				steered = true
			}
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
		}
	}
	if res == nil {
		t.Fatal("no result")
	}
	if res.FinalOutputString() != "revised" {
		t.Errorf("final = %q, want the steered answer", res.FinalOutputString())
	}
	if !strings.Contains(inputTexts(model.lastReq.Input), "do it differently") {
		t.Errorf("the steer never reached the model: %s", inputTexts(model.lastReq.Input))
	}
}

// NextTurn rides along with a turn the run was going to take anyway.
func TestNextTurn_RidesAlongWithTheNextTurn(t *testing.T) {
	tool := NewFunctionTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	if err := ctrl.NextTurn("also mention the weather"); err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(inputTexts(model.lastReq.Input), "mention the weather") {
		t.Errorf("the next-turn input never reached the model: %s", inputTexts(model.lastReq.Input))
	}
	if p := ctrl.Pending(); !p.Empty() {
		t.Errorf("input still queued after delivery: %+v", p)
	}
}

// Unlike Steer, NextTurn never extends a run that is finishing — and what it
// could not deliver is reported rather than silently dropped.
func TestNextTurn_DoesNotExtendAFinishingRun(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	if err := ctrl.NextTurn("too late"); err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — NextTurn must not force a turn", model.calls)
	}
	p := ctrl.Pending()
	if len(p.NextTurn) != 1 {
		t.Errorf("undelivered input = %+v, want it reported rather than dropped", p)
	}
}

// FollowUp continues the same run, so the trace, the usage total and the
// session stay one thing.
func TestFollowUp_ContinuesTheSameRun(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "first answer")),
		modelResp(messageOutput(t, "second answer")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	if err := ctrl.FollowUp("and what about tomorrow?"); err != nil {
		t.Fatal(err)
	}
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
		}
	}
	if res == nil {
		t.Fatal("no result")
	}
	if res.FinalOutputString() != "second answer" {
		t.Errorf("final = %q, want the follow-up's answer", res.FinalOutputString())
	}
	if len(res.RawResponses) != 2 {
		t.Errorf("raw responses = %d, want 2 in ONE run", len(res.RawResponses))
	}
	if !strings.Contains(inputTexts(model.lastReq.Input), "about tomorrow") {
		t.Errorf("the follow-up never reached the model: %s", inputTexts(model.lastReq.Input))
	}
	// The first answer is still part of the run's history.
	if !strings.Contains(inputTexts(model.lastReq.Input), "first answer") {
		t.Error("the follow-up turn lost the exchange that preceded it")
	}
}

// Injected input is persisted as the user's, so a reopened session shows what
// was actually said rather than an answer to a question nobody asked.
func TestInjectedInput_IsSavedToTheSession(t *testing.T) {
	ctx := context.Background()
	storage := NewInMemoryStorage("test")
	sess := NewSession(storage)
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "first")),
		modelResp(messageOutput(t, "second")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	stream, ctrl := Run(ctx, agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	})
	if err := ctrl.FollowUp("follow-up question"); err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}

	entries, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Source.Type == SourceUser && strings.Contains(string(e.Item), "follow-up question") {
			found = true
		}
	}
	if !found {
		t.Error("the injected follow-up was never saved to the session")
	}
}

func TestPendingInput_Empty(t *testing.T) {
	if !(PendingInput{}).Empty() {
		t.Error("a zero PendingInput is not empty")
	}
	if (PendingInput{Steer: make([]TResponseInputItem, 1)}).Empty() {
		t.Error("a queued steer reports empty")
	}
}

// Input queued before a run pauses must survive the pause: a steer sent while
// the run was working, then an approval that takes a human a minute, must not
// silently drop what was said.
func TestPendingInput_SurvivesAnInterruption(t *testing.T) {
	tool := NewFunctionTool("act", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "acted", nil
	})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "act", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		// Say something while the run is still going, before it pauses.
		if it, ok := ev.(*RunItemStreamEvent); ok {
			if _, isCall := it.Item.(*ToolCallItem); isCall {
				if err := ctrl.Steer("be careful with that"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
		}
	}
	if res == nil || len(res.Interruptions) != 1 {
		t.Fatalf("expected one interruption, got %+v", res)
	}
	// The state captured it without the caller doing anything.
	if len(res.State.PendingInput.Steer) != 1 {
		t.Fatalf("the paused state dropped the queued steer: %+v", res.State.PendingInput)
	}

	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.FinalOutputString() != "done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
	if !strings.Contains(inputTexts(model.lastReq.Input), "be careful") {
		t.Errorf("the steer did not survive the pause: %s", inputTexts(model.lastReq.Input))
	}
}
