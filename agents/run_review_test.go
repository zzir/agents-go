package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// A Blocking input guardrail's Replace verdict must reach the model on the
// guarded call itself. The input is built before the guardrail runs (so the
// guardrail can read rc.TurnInput()), and rebuilt from the replacement when
// one is returned — leaving the pre-replace build in place once sent the
// original input while the run result claimed it was replaced.
func TestBlockingGuardrailReplaceReachesModel(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:     "scrub",
			Stages:   []GuardrailStage{StageInput},
			Blocking: true,
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Replace("SCRUBBED", nil), nil
			},
		}},
	}
	if _, err := RunSync(context.Background(), agent, "the secret input", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	sent := ""
	for _, it := range model.lastReq.Input {
		raw, _ := session.MarshalInputItem(it)
		sent += string(raw)
	}
	if strings.Contains(sent, "the secret input") {
		t.Errorf("the model was sent the original input despite Replace: %s", sent)
	}
	if !strings.Contains(sent, "SCRUBBED") {
		t.Errorf("the model did not receive the replacement: %s", sent)
	}
}

// Racing (non-blocking) input guardrails must not de-stream the model call: a
// streamed run still yields raw response events on its first turn. The race
// once downgraded the call to a blocking Respond, and a chat UI with any
// input guardrail streamed nothing.
func TestStreamedRunWithRacingGuardrailKeepsRawEvents(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "peek",
			Stages: []GuardrailStage{StageInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Allow(nil), nil
			},
		}},
	}
	stream, _ := Run(context.Background(), agent, "hi", RunOptions{})
	raw := 0
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(*RawResponsesStreamEvent); ok {
			raw++
		}
	}
	if raw == 0 {
		t.Fatal("a streamed run with a racing input guardrail yielded no raw response events")
	}
}

// A truncated response must neither run its tool calls nor PAUSE on them: an
// approval put in front of a human would execute, on a cross-process resume, a
// call whose arguments may stop mid-JSON — the exact thing the truncation
// guard refuses in-process.
func TestTruncatedResponseDoesNotPause(t *testing.T) {
	executed := false
	tool := NewTool("act", "acts",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			executed = true
			return "ran", nil
		})
	tool.NeedsApproval = true
	truncated := modelResp(functionCallOutput(t, "act", "c1", `{"pa`))
	truncated.Status = "incomplete"
	truncated.IncompleteReason = "max_output_tokens"
	model := &fakeModel{responses: []*ModelResponse{
		truncated,
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 0 {
		t.Fatalf("a truncated response paused for approval: %v", res.Interruptions)
	}
	if executed {
		t.Fatal("a truncated response's tool call executed")
	}
}

// Truncated() must survive RunState serialization: a resumed-in-another-process
// run refuses the same calls the pausing process would have.
func TestSerializedResponseKeepsTruncation(t *testing.T) {
	resp := modelResp(messageOutput(t, "x"))
	resp.Status = "incomplete"
	resp.IncompleteReason = "max_output_tokens"
	if !resp.Truncated() {
		t.Fatal("test setup: response not truncated")
	}
	got, err := deserializeResponse(serializeResponse(resp))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated() {
		t.Fatal("truncation lost across serialization; a cross-process resume would execute refused calls")
	}
}

// A resumed turn re-processes a response whose usage the pausing segment
// already attributed at its save point; re-arming attribution stamps the same
// request onto the resumed batch — a request counted twice.
func TestResumeDoesNotReattributeUsage(t *testing.T) {
	ctx := context.Background()
	tool := NewTool("act", "acts",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	tool.NeedsApproval = true
	first := modelResp(functionCallOutput(t, "act", "c1", `{}`))
	first.ResponseID = "resp_1"
	first.Usage = &Usage{Requests: 1, InputTokens: 5, OutputTokens: 3, TotalTokens: 8}
	second := modelResp(messageOutput(t, "done"))
	second.ResponseID = "resp_2"
	second.Usage = &Usage{Requests: 1, InputTokens: 7, OutputTokens: 2, TotalTokens: 9}
	model := &fakeModel{responses: []*ModelResponse{first, second}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
	sess := session.NewInMemorySession()

	res, err := RunSync(ctx, agent, "go", RunOptions{Conversation: ConversationOptions{Session: sess}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("want an approval pause, got %v", res.Interruptions)
	}
	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRunSync(ctx, res.State, RunOptions{Conversation: ConversationOptions{Session: sess}}); err != nil {
		t.Fatal(err)
	}

	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var in, out int64
	for _, e := range entries {
		if e.Usage != nil {
			in += e.Usage.InputTokens
			out += e.Usage.OutputTokens
		}
	}
	// Each request attributed exactly once: 5+7 in, 3+2 out.
	if in != 12 || out != 5 {
		t.Fatalf("session entries carry %d/%d tokens, want 12/5 — a request was attributed twice", in, out)
	}
}

// Abandoning a streamed run must stop it WHERE IT STANDS, including when a
// racing input guardrail is still in flight: waiting for the guardrail parked
// the consumer's break for its full duration, and forever for one that returns
// only on cancellation.
func TestAbandonedStreamDoesNotWaitForRacingGuardrail(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "slow",
			Stages: []GuardrailStage{StageInput},
			Run: func(ctx context.Context, _ *RunContext, _ GuardrailPayload) (GuardrailDecision, error) {
				// Returns only when cancelled, which is what a guardrail
				// waiting on a remote moderation call looks like when the run
				// is abandoned.
				close(started)
				<-ctx.Done()
				close(released)
				return Allow(nil), ctx.Err()
			},
		}},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, _ := Run(context.Background(), agent, "hi", RunOptions{})
		for range stream {
			break
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoning the stream blocked on the racing input guardrail")
	}
	// If the guardrail got as far as running, abandoning the run must also
	// have cancelled it — otherwise its goroutine outlives the run forever.
	// (Cancelled before it started is equally fine and is what usually
	// happens, since the abandon cancels immediately.)
	select {
	case <-started:
		select {
		case <-released:
		case <-time.After(5 * time.Second):
			t.Fatal("the racing guardrail ran and was never cancelled; its goroutine outlives the run")
		}
	default:
	}
}

// A pause's queued input survives retried attempts exactly once. Delivery is
// transactional: taking moves input in flight, a failed attempt rolls it
// back, a durable one commits — the transaction, not reseeding, is what makes
// a retry deliver input neither zero times nor twice. restore itself seeds
// once per control.
func TestRestore_TransactionalDeliveryAcrossRetries(t *testing.T) {
	ctrl := newRunControl()
	pending := PendingInput{Steer: InputItemsFromText("USE STAGING")}

	// Seeding is once per control: a retried attempt's second restore must
	// not duplicate what is still queued.
	ctrl.restore(pending)
	ctrl.restore(pending)
	if got := len(ctrl.Pending().Steer); got != 1 {
		t.Fatalf("queued steer = %d, want 1 — a retry duplicated the human's input", got)
	}

	// Attempt 1 consumes and fails: the rollback returns the steer, and the
	// next attempt's restore stays a no-op — nothing lost, nothing doubled.
	if got := len(ctrl.takeTurnInput()); got != 1 {
		t.Fatal("taking the turn input did not drain the steer")
	}
	ctrl.rollbackInjected()
	ctrl.restore(pending)
	if got := len(ctrl.Pending().Steer); got != 1 {
		t.Fatalf("queued steer = %d after a failed attempt, want 1 — the steer was lost", got)
	}

	// Attempt 2 consumes and delivers: after the commit a restore must not
	// resurrect input whose durable home already exists.
	if got := len(ctrl.takeTurnInput()); got != 1 {
		t.Fatal("second take did not drain the rolled-back steer")
	}
	ctrl.commitInjected()
	ctrl.restore(pending)
	if got := len(ctrl.Pending().Steer); got != 0 {
		t.Fatalf("queued steer = %d after delivery, want 0 — a reseed doubled delivered input", got)
	}
}
