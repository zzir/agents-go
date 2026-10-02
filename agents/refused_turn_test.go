package agents_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

const orderSecret = "card ending 4242"

// placeOrder ends the run on its own result and counts how often it ran.
func placeOrder(ran *int) *agents.Tool {
	return agents.NewTool("place_order", "places the order",
		func(context.Context, *agents.ToolContext, struct{}) (agents.ToolResult, error) {
			*ran++
			return agents.ToolResult{
				Content:   []agents.ToolOutputContent{agents.ToolOutputText{Text: "order placed, " + orderSecret}},
				Summary:   "paid with " + orderSecret,
				Terminate: true,
			}, nil
		})
}

func noCardNumbers() agents.Guardrail {
	return agents.NewOutputGuardrail("no_card_numbers", func(_ context.Context, output any) (agents.GuardrailDecision, error) {
		if s, _ := output.(string); strings.Contains(s, "card ending") {
			return agents.Trip("card number"), nil
		}
		return agents.Allow(nil), nil
	})
}

// storedKinds lists what the session holds: the item type of an item entry,
// in order.
func storedKinds(t *testing.T, sess *session.Session) []string {
	t.Helper()
	items, err := sess.ContextItems(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(items))
	for _, it := range items {
		switch {
		case it.OfFunctionCall != nil:
			kinds = append(kinds, "function_call")
		case it.OfFunctionCallOutput != nil:
			kinds = append(kinds, "function_call_output")
		case it.OfReasoning != nil:
			kinds = append(kinds, "reasoning")
		default:
			kinds = append(kinds, "message")
		}
	}
	return kinds
}

func sessionCarries(t *testing.T, sess *session.Session, text string) bool {
	t.Helper()
	entries, err := sess.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(string(e.Item), text) {
			return true
		}
		if e.Display != nil && strings.Contains(e.Display.Output+e.Display.Summary, text) {
			return true
		}
	}
	return false
}

// A Terminate turn an output guardrail trips on is saved whole, its tool
// output withheld (spec §2.5): the next run in the session sees that the tool
// ran, and never what it returned.
func TestTrippedTerminateTurnPersistsWithheldCalls(t *testing.T) {
	ctx := context.Background()
	model := agentstest.NewResponseBuilder().
		Reasoning("the user wants it placed").
		Text("placing it now").
		FunctionCall("place_order", "call-1", "{}").
		NewTurn().
		Text("already placed").
		Build()
	ran := 0
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools:      []*agents.Tool{placeOrder(&ran)},
		Guardrails: []agents.Guardrail{noCardNumbers()},
	}
	sess := session.NewInMemorySession()
	opts := agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}}

	stream, _ := agents.Run(ctx, agent, "place it", opts)
	var announced bool
	var runErr error
	for ev, err := range stream {
		if err != nil {
			runErr = err
			break
		}
		if _, ok := ev.(*agents.ItemsPersistedEvent); ok {
			announced = len(storedKinds(t, sess)) > 1
		}
	}
	if _, ok := errors.AsType[*agents.GuardrailTripwireError](runErr); !ok {
		t.Fatalf("err = %v, want the tripwire", runErr)
	}
	if ran != 1 {
		t.Fatalf("tool ran %d times, want once", ran)
	}
	want := []string{"message", "reasoning", "message", "function_call", "function_call_output"}
	if got := storedKinds(t, sess); !slices.Equal(got, want) {
		t.Fatalf("session = %v, want %v", got, want)
	}
	if !sessionCarries(t, sess, "Output withheld") || sessionCarries(t, sess, orderSecret) {
		t.Error("the session misses the withheld notice, or holds what the guardrail refused")
	}
	if !announced {
		t.Error("the withheld save was not announced as ItemsPersistedEvent")
	}
	// The caller still reads what the tool returned.
	rerr, _ := errors.AsType[*agents.RunError](runErr)
	if rerr == nil || !slices.ContainsFunc(rerr.Result.NewItems, func(it *agents.RunItem) bool {
		return it.Kind == agents.ItemToolCallOutput && strings.Contains(it.Display().Output, orderSecret)
	}) {
		t.Error("RunError.Result lost the tool's real output")
	}

	// The next run is told the tool ran: its request replays the call and the notice.
	plain := &agents.Agent{Name: "a", ModelImpl: model, Tools: agent.Tools}
	if _, err := agents.RunSync(ctx, plain, "did it go through?", opts); err != nil {
		t.Fatal(err)
	}
	var sawCall, sawNotice bool
	for _, item := range model.LastRequest().Input {
		sawCall = sawCall || (item.OfFunctionCall != nil && item.OfFunctionCall.CallID == "call-1")
		if out := item.OfFunctionCallOutput; out != nil {
			sawNotice = sawNotice || strings.Contains(out.Output.OfString.Value, "Output withheld")
		}
	}
	if !sawCall || !sawNotice {
		t.Errorf("next run saw the call = %v, the notice = %v; want both", sawCall, sawNotice)
	}
	if ran != 1 {
		t.Errorf("tool ran %d times across both runs, want once", ran)
	}
}

// A final turn that ran no tool leaves nothing when its output is refused.
func TestTrippedMessageTurnPersistsNothing(t *testing.T) {
	model := agentstest.TextModel("your " + orderSecret)
	agent := &agents.Agent{Name: "a", ModelImpl: model, Guardrails: []agents.Guardrail{noCardNumbers()}}
	sess := session.NewInMemorySession()
	_, err := agents.RunSync(context.Background(), agent, "which card?", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if _, ok := errors.AsType[*agents.GuardrailTripwireError](err); !ok {
		t.Fatalf("err = %v, want the tripwire", err)
	}
	if got := storedKinds(t, sess); !slices.Equal(got, []string{"message"}) {
		t.Errorf("session = %v, want the user message alone", got)
	}
}

// OnEnd failing a Terminate turn is the same refusal: the tool ran, and the
// session says so.
func TestOnEndFailureKeepsWithheldCalls(t *testing.T) {
	model := agentstest.NewResponseBuilder().FunctionCall("place_order", "call-1", "{}").Build()
	ran := 0
	hookErr := errors.New("audit sink down")
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools: []*agents.Tool{placeOrder(&ran)},
		OnEnd: func(context.Context, *agents.RunContext, any) error { return hookErr },
	}
	sess := session.NewInMemorySession()
	_, err := agents.RunSync(context.Background(), agent, "place it", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("err = %v, want the hook's", err)
	}
	want := []string{"message", "function_call", "function_call_output"}
	if got := storedKinds(t, sess); !slices.Equal(got, want) {
		t.Fatalf("session = %v, want %v", got, want)
	}
	if sessionCarries(t, sess, orderSecret) {
		t.Error("the session holds the output of a turn the run refused")
	}
}

// A save that fails on top of the refusal keeps the refusal first, so the
// caller still finds the tripwire.
func TestRefusedTurnSaveFailureKeepsTripwire(t *testing.T) {
	model := agentstest.NewResponseBuilder().FunctionCall("place_order", "call-1", "{}").Build()
	ran := 0
	agent := &agents.Agent{
		Name: "a", ModelImpl: model,
		Tools:      []*agents.Tool{placeOrder(&ran)},
		Guardrails: []agents.Guardrail{noCardNumbers()},
	}
	storeErr := errors.New("disk full")
	st := &failAfterStorage{Storage: session.NewInMemoryStorage("mem"), allowed: 1, err: storeErr}
	_, err := agents.RunSync(context.Background(), agent, "place it", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: session.NewSession(st)},
	})
	if _, ok := errors.AsType[*agents.GuardrailTripwireError](err); !ok {
		t.Fatalf("err = %v, want the tripwire reachable", err)
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("err = %v, want the failed save reported too", err)
	}
}

// failAfterStorage accepts a fixed number of appends, then fails every one.
type failAfterStorage struct {
	session.Storage
	allowed int
	err     error
}

func (s *failAfterStorage) Append(ctx context.Context, entries ...session.Entry) error {
	if s.allowed == 0 {
		return s.err
	}
	s.allowed--
	return s.Storage.Append(ctx, entries...)
}
