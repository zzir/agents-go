package agents_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// noForbidden trips on input carrying "forbidden" and softens input carrying
// "rude"; it passes everything else, the run's first input included.
func noForbidden(blocking bool) agents.Guardrail {
	g := agents.NewInputGuardrail("no_forbidden", func(_ context.Context, input []agents.InputItem) (agents.GuardrailDecision, error) {
		for _, item := range input {
			switch text := session.ItemText(item); {
			case strings.Contains(text, "forbidden"):
				return agents.Trip("forbidden word"), nil
			case strings.Contains(text, "rude"):
				return agents.Replace("please, a softer ask", nil), nil
			}
		}
		return agents.Allow(nil), nil
	})
	g.Blocking = blocking
	return g
}

// steeringTool queues input through the run's control while the first turn runs.
func steeringTool(ctrl *agents.RunControl, queue func(agents.RunControl) error) *agents.Tool {
	return agents.NewTool("probe", "probes",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			return "ok", queue(*ctrl)
		})
}

func requestsCarry(model *agentstest.FakeModel, text string) bool {
	for _, req := range model.Requests() {
		for _, item := range req.Input {
			if strings.Contains(session.ItemText(item), text) {
				return true
			}
		}
	}
	return false
}

// A steer is screened like the input the run started with (spec §2.6): one a
// guardrail trips on fails the run, reaches neither the model nor the session,
// and is consumed — the verdict holds it, Pending does not.
func TestSteerTripsInputGuardrail(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("probe", "call-1", "{}").
		NewTurn().
		Text("never asked").
		Build()
	var ctrl agents.RunControl
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools:      []*agents.Tool{steeringTool(&ctrl, func(c agents.RunControl) error { return c.Steer("a forbidden ask") })},
		Guardrails: []agents.Guardrail{noForbidden(true)},
	}
	sess := session.NewInMemorySession()
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	ctrl = c
	_, err := stream.Collect()
	trip, ok := errors.AsType[*agents.GuardrailTripwireError](err)
	if !ok {
		t.Fatalf("err = %v, want the tripwire", err)
	}
	checked, _ := trip.Result.Checked.([]agents.InputItem)
	if len(checked) != 1 || !strings.Contains(session.ItemText(checked[0]), "forbidden") {
		t.Errorf("Checked = %v, want the refused steer and nothing else", trip.Result.Checked)
	}
	if !ctrl.Pending().Empty() {
		t.Errorf("pending = %+v, want the refused steer consumed", ctrl.Pending())
	}
	if savedFromUser(t, sess, "forbidden") {
		t.Error("the refused steer was persisted")
	}
	if model.Calls() != 1 || requestsCarry(model, "forbidden") {
		t.Errorf("model calls = %d, saw the steer = %v; want one call that never saw it", model.Calls(), requestsCarry(model, "forbidden"))
	}
	// The turn that ran before the steer was taken is saved.
	if !savedFromUser(t, sess, "go") {
		t.Error("the turn before the refused steer was not persisted")
	}
}

// A Replace verdict swaps the injected input for its message before the model
// or the session sees any of it.
func TestSteerReplacedByInputGuardrail(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("probe", "call-1", "{}").
		NewTurn().
		Text("done").
		Build()
	var ctrl agents.RunControl
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools:      []*agents.Tool{steeringTool(&ctrl, func(c agents.RunControl) error { return c.Steer("a rude ask") })},
		Guardrails: []agents.Guardrail{noForbidden(false)},
	}
	sess := session.NewInMemorySession()
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	ctrl = c
	res, err := stream.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if !injected(res.NewItems, "softer ask") || injected(res.NewItems, "rude") {
		t.Errorf("injected items carry the original, not the replacement: %v", agentstest.ItemTypes(res.NewItems))
	}
	if !savedFromUser(t, sess, "softer ask") || savedFromUser(t, sess, "rude") {
		t.Error("the session holds the original steer, or misses the replacement")
	}
	if !requestsCarry(model, "softer ask") || requestsCarry(model, "rude") {
		t.Error("the model saw the original steer, or missed the replacement")
	}
}

// A follow-up taken at the final output is screened too. One that trips fails
// the run after the answer it followed is saved.
func TestFollowUpScreened(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("probe", "call-1", "{}").
		NewTurn().
		Text("the answer").
		NewTurn().
		Text("never asked").
		Build()
	var ctrl agents.RunControl
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools:      []*agents.Tool{steeringTool(&ctrl, func(c agents.RunControl) error { return c.FollowUp("a forbidden follow-up") })},
		Guardrails: []agents.Guardrail{noForbidden(false)},
	}
	sess := session.NewInMemorySession()
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	ctrl = c
	_, err := stream.Collect()
	if _, ok := errors.AsType[*agents.GuardrailTripwireError](err); !ok {
		t.Fatalf("err = %v, want the tripwire", err)
	}
	if model.Calls() != 2 {
		t.Errorf("model calls = %d, want the two before the follow-up", model.Calls())
	}
	if !ctrl.Pending().Empty() || savedFromUser(t, sess, "forbidden") {
		t.Errorf("the refused follow-up is pending (%+v) or persisted", ctrl.Pending())
	}
	entries, eerr := sess.Entries(context.Background(), session.Cursor{})
	if eerr != nil {
		t.Fatal(eerr)
	}
	answered := false
	for _, e := range entries {
		if strings.Contains(string(e.Item), "the answer") {
			answered = true
		}
	}
	if !answered {
		t.Error("the answer the follow-up came after was not persisted")
	}
}

// Injected input waits for its guardrails whatever Blocking says: no model
// call is made while one is still deciding.
func TestNonBlockingGuardrailGatesInjection(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("probe", "call-1", "{}").
		NewTurn().
		Text("done").
		Build()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	slow := agents.NewInputGuardrail("slow", func(ctx context.Context, input []agents.InputItem) (agents.GuardrailDecision, error) {
		for _, item := range input {
			if strings.Contains(session.ItemText(item), "and the date") {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return agents.GuardrailDecision{}, ctx.Err()
				}
			}
		}
		return agents.Allow(nil), nil
	})
	var ctrl agents.RunControl
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools:      []*agents.Tool{steeringTool(&ctrl, func(c agents.RunControl) error { return c.Steer("and the date") })},
		Guardrails: []agents.Guardrail{slow},
	}
	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{})
	ctrl = c
	done := make(chan error, 1)
	go func() {
		_, err := stream.Collect()
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the guardrail never saw the steer")
	}
	// The guardrail is deciding: the second model call must not have started.
	time.Sleep(50 * time.Millisecond)
	if calls := model.Calls(); calls != 1 {
		t.Fatalf("model calls = %d while the guardrail was deciding, want 1", calls)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish after the guardrail let the steer through")
	}
	if model.Calls() != 2 {
		t.Errorf("model calls = %d, want 2", model.Calls())
	}
}
