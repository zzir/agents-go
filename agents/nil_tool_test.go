package agents

import (
	"context"
	"strings"
	"testing"
)

// A nil entry in Agent.Tools is a construction bug (a conditional append gone
// wrong); the run reports it by name instead of panicking on a field read.
func TestRun_NilToolEntryIsNamed(t *testing.T) {
	agent := &Agent{Name: "a", Tools: []*Tool{nil}, ModelImpl: &fakeModel{}}
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "Tools[0] is nil") {
		t.Fatalf("err = %v, want a named nil-tool error", err)
	}
}

// A tool with no OnInvoke is still advertised and still dispatched: the model
// calling it must fail as a UserError naming the tool — a configuration bug —
// not as "tool not found", which blames the model and, under
// ToolNotFoundReturnToModel, invites it to retry forever against one.
func TestRun_NilOnInvokeIsAUserErrorNotToolNotFound(t *testing.T) {
	agent := &Agent{Name: "a", Tools: []*Tool{{Name: "broken"}}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "broken", "c1", `{}`)),
	}}}
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "has no OnInvoke") {
		t.Fatalf("err = %v, want the no-OnInvoke UserError", err)
	}
	if CodeOf(err) != CodeUserError {
		t.Fatalf("CodeOf = %v, want user_error (not model_behavior)", CodeOf(err))
	}
}
