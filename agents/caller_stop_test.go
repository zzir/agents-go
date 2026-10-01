package agents_test

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// savedFromUser reports whether the session holds a user entry carrying text.
func savedFromUser(t *testing.T, sess *session.Session, text string) bool {
	t.Helper()
	entries, err := sess.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Source.Type == agents.SourceUser && strings.Contains(string(e.Item), text) {
			return true
		}
	}
	return false
}

// injected reports whether items hold an injected input carrying text.
func injected(items []*agents.RunItem, text string) bool {
	for _, it := range items {
		if it.Kind == agents.ItemInjectedInput && it.RawInput != nil && strings.Contains(session.ItemText(*it.RawInput), text) {
			return true
		}
	}
	return false
}

// A steer queued on the turn the caller stops at is not consumed: it is in
// Pending, in no result and in no session.
func TestCallerStopLeavesSteerPending(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("probe", "call-1", "{}").
		NewTurn().
		Text("never asked").
		Build()
	var ctrl agents.RunControl
	probe := agents.NewTool("probe", "probes",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			err := ctrl.Steer("and the date")
			ctrl.StopAfterTurn()
			return "ok", err
		})
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{probe}}
	sess := session.NewInMemorySession()
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	ctrl = c
	res, err := stream.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if !res.StoppedEarly || res.FinalOutput != nil {
		t.Errorf("StoppedEarly=%v FinalOutput=%v, want a stopped run with no output", res.StoppedEarly, res.FinalOutput)
	}
	if got := len(ctrl.Pending().Steer); got != 1 {
		t.Errorf("pending steers = %d, want the one the stop left queued", got)
	}
	if savedFromUser(t, sess, "and the date") || injected(res.NewItems, "and the date") {
		t.Error("the steer was consumed although the caller stopped")
	}
	if model.Calls() != 1 {
		t.Errorf("model calls = %d, want 1", model.Calls())
	}
}

// onCallModel runs a hook as each model call starts.
type onCallModel struct {
	agents.Model
	onCall func()
}

func (m onCallModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	m.onCall()
	return m.Model.Respond(ctx, req)
}

func (m onCallModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	m.onCall()
	return m.Model.StreamResponse(ctx, req)
}

// A stop asked for during the call that produces the final output keeps that
// output; the steer that arrived with it stays queued.
func TestCallerStopAtFinalOutputKeepsLateSteerPending(t *testing.T) {
	model := agentstest.TextModel("done", "never asked")
	var ctrl agents.RunControl
	agent := &agents.Agent{Name: "a", ModelImpl: onCallModel{Model: model, onCall: func() {
		if err := ctrl.Steer("too late"); err != nil {
			t.Error(err)
		}
		ctrl.StopAfterTurn()
	}}}
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: session.NewInMemorySession()},
	})
	ctrl = c
	res, err := stream.Collect()
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "done")
	if !res.StoppedEarly {
		t.Error("StoppedEarly must report the stop the caller asked for")
	}
	if got := len(ctrl.Pending().Steer); got != 1 {
		t.Errorf("pending steers = %d, want the late one", got)
	}
	if model.Calls() != 1 {
		t.Errorf("model calls = %d, want 1", model.Calls())
	}
}

// A stop that arrives after the save point drained the queue still ends the
// run, and the drained steer is saved with the turn rather than lost.
func TestCallerStopAfterDrainPersistsSteer(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("probe", "call-1", "{}").
		NewTurn().
		Text("never asked").
		Build()
	var ctrl agents.RunControl
	probe := agents.NewTool("probe", "probes",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			return "ok", ctrl.Steer("and the date")
		})
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{probe}}
	sess := session.NewInMemorySession()
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Exec: agents.ExecOptions{PrepareNextTurn: func(context.Context, *agents.TurnResult) (*agents.TurnSnapshot, error) {
			ctrl.StopAfterTurn()
			return nil, nil
		}},
	})
	ctrl = c
	res, err := stream.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if !res.StoppedEarly {
		t.Error("StoppedEarly must report the stop the caller asked for")
	}
	if !savedFromUser(t, sess, "and the date") || !injected(res.NewItems, "and the date") {
		t.Error("the drained steer must be in the session and in the result")
	}
	if !ctrl.Pending().Empty() {
		t.Errorf("pending = %+v, want nothing left queued", ctrl.Pending())
	}
	if model.Calls() != 1 {
		t.Errorf("model calls = %d, want 1", model.Calls())
	}
}
