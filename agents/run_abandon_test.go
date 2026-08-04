package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRun_AbandonedStreamCancelsRunningTools pins spec §2.0's "stops the run
// where it stands" to the tool batch: when the consumer stops ranging while
// tools are executing, their context is cancelled — the loop cannot observe
// the departure itself while blocked in the batch wait, so emit must
// propagate it.
func TestRun_AbandonedStreamCancelsRunningTools(t *testing.T) {
	toolCancelled := make(chan struct{})
	tool := NewFunctionTool("slow", "blocks until cancelled",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			tc.Emit(TextResult("working"))
			select {
			case <-ctx.Done():
				close(toolCancelled)
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "finished anyway", nil
			}
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "slow", "call_1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{})
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(*ToolProgressEvent); ok {
			break // abandon the run mid-tool
		}
	}
	select {
	case <-toolCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("tool context was not cancelled after the consumer abandoned the stream")
	}
}

// TestRun_FailedModelCallCancelsRacingGuardrail pins the teardown of the
// first-turn guardrail race: once the model call has failed on its own, the
// racing (non-blocking) input guardrails are cancelled rather than run to
// completion — a slow LLM-based guardrail must not hold an already-failed run
// open for its full duration.
func TestRun_FailedModelCallCancelsRacingGuardrail(t *testing.T) {
	slow := Guardrail{
		Name:   "slow",
		Stages: []GuardrailStage{StageInput},
		Run: func(ctx context.Context, rc *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			select {
			case <-ctx.Done():
				return GuardrailDecision{}, ctx.Err()
			case <-time.After(5 * time.Second):
				return GuardrailDecision{}, nil
			}
		},
	}
	agent := &Agent{Name: "a", ModelImpl: &failingModel{err: errors.New("model down")}, Guardrails: []Guardrail{slow}}

	start := time.Now()
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "model down") {
		t.Fatalf("err = %v, want the model failure", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("run held open %v by a racing guardrail after the model already failed", elapsed)
	}
}

// TestRunTools_SiblingCancellationDoesNotMaskRealError pins the batch's error
// attribution: the errgroup cancels the siblings after the first fatal
// failure, and a lower-index tool that honors the cancellation must not win
// the deterministic lowest-index pick over the error that caused it.
func TestRunTools_SiblingCancellationDoesNotMaskRealError(t *testing.T) {
	victim := NewFunctionTool("victim", "cancelled by the sibling's failure",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
	victim.FailureErrorFunction = nil // fatal errors abort the run
	culprit := NewFunctionTool("culprit", "the real failure",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "", errors.New("real boom")
		})
	culprit.FailureErrorFunction = nil
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(
			functionCallOutput(t, "victim", "call_v", `{}`),
			functionCallOutput(t, "culprit", "call_c", `{}`),
		),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{victim, culprit}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("expected the batch to fail")
	}
	if !strings.Contains(err.Error(), "real boom") {
		t.Errorf("err = %v, want the culprit's error, not the cancelled sibling's", err)
	}
}
