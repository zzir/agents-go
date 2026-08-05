package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// inputTexts renders a model request's input for assertions.
func inputTexts(items []InputItem) string {
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
			if m.Item.Kind == ItemMessage {
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
	tool := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

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
	if (PendingInput{Steer: make([]InputItem, 1)}).Empty() {
		t.Error("a queued steer reports empty")
	}
}

// Input queued before a run pauses must survive the pause: a steer sent while
// the run was working, then an approval that takes a human a minute, must not
// silently drop what was said.
func TestPendingInput_SurvivesAnInterruption(t *testing.T) {
	tool := NewTool("act", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "acted", nil
	})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "act", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		// Say something while the run is still going, before it pauses.
		if it, ok := ev.(*RunItemStreamEvent); ok {
			if it.Item.Kind == ItemToolCall {
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

// Injections reach the model in arrival order across kinds. The three-queue
// design replayed all steer before all next-turn regardless of arrival; for
// two messages from the same caller that reordering could invert meaning
// ("do X", then "actually, don't").
func TestInjection_ArrivalOrderAcrossKinds(t *testing.T) {
	ctrl := newRunControl()
	if err := ctrl.NextTurn("first"); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Steer("second"); err != nil {
		t.Fatal(err)
	}
	if got := inputTexts(ctrl.takeTurnInput()); got != "first|second|" {
		t.Fatalf("takeTurnInput order = %q, want arrival order %q", got, "first|second|")
	}
}

// A rollback returns in-flight input to its arrival position, not to the
// front: an early follow-up already queued stays ahead of a later steer that
// a failed attempt consumed.
func TestInjection_RollbackMergesByArrival(t *testing.T) {
	ctrl := newRunControl()
	if err := ctrl.FollowUp("early"); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Steer("late"); err != nil {
		t.Fatal(err)
	}
	// The save point takes only the steer; the follow-up is not its kind.
	if got := inputTexts(ctrl.takeTurnInput()); got != "late|" {
		t.Fatalf("takeTurnInput = %q, want %q", got, "late|")
	}
	ctrl.rollbackInjected()
	// After the failed attempt's rollback the continuation point sees both,
	// back in arrival order.
	if got := inputTexts(ctrl.takeContinuation()); got != "early|late|" {
		t.Fatalf("takeContinuation after rollback = %q, want %q", got, "early|late|")
	}
}

// Pending reports what is queued, not what an attempt holds in flight; a
// rollback puts it back, a commit settles it for good.
func TestInjection_PendingExcludesInFlight(t *testing.T) {
	ctrl := newRunControl()
	if err := ctrl.Steer("x"); err != nil {
		t.Fatal(err)
	}
	ctrl.takeTurnInput()
	if !ctrl.Pending().Empty() {
		t.Fatal("in-flight input still reported as pending")
	}
	ctrl.rollbackInjected()
	if got := len(ctrl.Pending().Steer); got != 1 {
		t.Fatalf("rolled-back steer not pending again, got %d", got)
	}
	ctrl.takeTurnInput()
	ctrl.commitInjected()
	if !ctrl.Pending().Empty() {
		t.Fatal("committed input still reported as pending")
	}
}

// appendFailingStorage fails Append when any entry in the batch contains the
// marker text, simulating a storage failure at a chosen persistence boundary.
type appendFailingStorage struct {
	SessionStorage
	failOn string
}

func (s *appendFailingStorage) Append(ctx context.Context, entries ...SessionEntry) error {
	for _, e := range entries {
		if strings.Contains(string(e.Item), s.failOn) {
			return errors.New("storage refused the batch")
		}
	}
	return s.SessionStorage.Append(ctx, entries...)
}

// A continuation take (follow-up/late steer at the final-output boundary) is
// committed only by a write that actually covers it. When that persist fails,
// the take must roll back into the queue — committing it against the write
// that merely preceded it would make a retrying attempt lose the input.
func TestContinuationTake_RollsBackWhenItsPersistFails(t *testing.T) {
	storage := &appendFailingStorage{SessionStorage: NewInMemoryStorage("test"), failOn: "about tomorrow"}
	sess := NewSession(storage)
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "first answer")),
		modelResp(messageOutput(t, "never reached")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	})
	if err := ctrl.FollowUp("and what about tomorrow?"); err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatal("the failing persist should have failed the run")
	}
	if p := ctrl.Pending(); len(p.FollowUp) == 0 {
		t.Error("the follow-up was consumed by the failed attempt and never rolled back")
	}
}

// An injection riding into a turn that pauses for approval is committed only
// once the pause's persist has succeeded (its durable home is then the
// RunState's item log). A persist failure fails the attempt before any
// RunState exists, so the take must roll back for the retry.
func TestInterruptionTake_RollsBackWhenPersistFails(t *testing.T) {
	probe := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	danger := NewTool("delete_db", "dangerous", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "deleted", nil
	})
	danger.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "call_1", `{}`)),
		modelResp(functionCallOutput(t, "delete_db", "call_2", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{probe, danger}, ModelImpl: model}
	storage := &appendFailingStorage{SessionStorage: NewInMemoryStorage("test"), failOn: "please also"}
	sess := NewSession(storage)

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	})
	// Taken at the save point after turn 1, so it rides into the turn that
	// pauses; its batch is the one the pause's persist writes — and refuses.
	if err := ctrl.Steer("please also check the backups"); err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range stream {
		if err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		t.Fatal("the failing persist should have failed the run")
	}
	if p := ctrl.Pending(); len(p.Steer) == 0 {
		t.Error("the steer was consumed by the failed pause and never rolled back")
	}
}

// Input queued on a resumed control before ranging begins must deliver AFTER
// the pre-pause backlog — the old input was said first, and restore seeds the
// queue before the control reaches the caller.
func TestResume_PreRangeSteerOrdersAfterRestoredBacklog(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	model := agent.ModelImpl.(*fakeModel)

	stream, ctrl := Run(context.Background(), agent, "delete it", RunOptions{})
	// Said while the run was working; the pause arrives before any save point,
	// so it travels in RunState.PendingInput.
	if err := ctrl.Steer("said before the pause"); err != nil {
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
	if res == nil || res.State == nil || len(res.Interruptions) != 1 {
		t.Fatal("expected an interrupted run with state")
	}

	res.State.Approve(res.Interruptions[0], false)
	stream2, ctrl2 := ResumeRun(context.Background(), res.State, RunOptions{})
	// Before ranging begins — legal, and it must order after the backlog.
	if err := ctrl2.Steer("said after the resume"); err != nil {
		t.Fatal(err)
	}
	for _, err := range stream2 {
		if err != nil {
			t.Fatal(err)
		}
	}
	in := inputTexts(model.lastReq.Input)
	i, j := strings.Index(in, "said before the pause"), strings.Index(in, "said after the resume")
	if i < 0 || j < 0 {
		t.Fatalf("both steers must reach the model: %s", in)
	}
	if i > j {
		t.Errorf("pre-pause input delivered after the post-resume one: %s", in)
	}
}
