package agents

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// itemsContain reports whether any item's wire JSON contains needle.
func itemsContain(items []TResponseInputItem, needle string) bool {
	for _, it := range items {
		b, err := MarshalInputItem(it)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}

// TurnInput must be the input actually sent to the model, which includes
// session history loaded before the run. It used to be reconstructed from the
// run's own items only, so prior history was missing.
func TestTurnInput_IncludesSessionHistory(t *testing.T) {
	session := NewInMemorySession()
	prior, err := UnmarshalInputItem([]byte(`{"role":"user","content":"MARKER_FROM_HISTORY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddItems(context.Background(), []TResponseInputItem{prior}); err != nil {
		t.Fatal(err)
	}

	var seen []TResponseInputItem
	tool := NewFunctionTool("probe", "probe",
		func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
			seen = tc.TurnInput()
			return "ok", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := Run(context.Background(), agent, "new question", RunOptions{Session: session}); err != nil {
		t.Fatal(err)
	}
	if !itemsContain(seen, "MARKER_FROM_HISTORY") {
		t.Errorf("TurnInput is missing the session history that was sent to the model; got %d items", len(seen))
	}
	if !itemsContain(seen, "new question") {
		t.Error("TurnInput is missing the run's new input")
	}
}

// TurnInput reflects CallModelInputFilter edits, because it reports what was
// sent rather than what the runner assembled.
func TestTurnInput_ReflectsModelInputFilter(t *testing.T) {
	var seen []TResponseInputItem
	tool := NewFunctionTool("probe", "probe",
		func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
			seen = tc.TurnInput()
			return "ok", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	_, err := Run(context.Background(), agent, "original", RunOptions{
		CallModelInputFilter: func(_ context.Context, _ *RunContext, _ *Agent, d ModelInputData) (ModelInputData, error) {
			d.Input = InputItemsFromText("REWRITTEN_BY_FILTER")
			return d, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !itemsContain(seen, "REWRITTEN_BY_FILTER") {
		t.Error("TurnInput does not reflect the model-input filter's rewrite")
	}
	if itemsContain(seen, "original") {
		t.Error("TurnInput still carries the pre-filter input")
	}
}

// The turn's input is published before the model call, so guardrails inspecting
// the run context see it too.
func TestTurnInput_VisibleToGuardrails(t *testing.T) {
	var mu sync.Mutex
	var seen []TResponseInputItem
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:     "peek",
			Stages:   []GuardrailStage{StageInput},
			Blocking: true,
			Run: func(_ context.Context, rc *RunContext, _ GuardrailPayload) (GuardrailDecision, error) {
				mu.Lock()
				seen = rc.TurnInput()
				mu.Unlock()
				return Allow(nil), nil
			},
		}},
	}
	if _, err := Run(context.Background(), agent, "guarded question", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !itemsContain(seen, "guarded question") {
		t.Errorf("guardrail saw %d turn-input items; want the run's input", len(seen))
	}
}

// TurnInput advances with the turn: the second turn's view includes the first
// turn's tool call and its output.
func TestTurnInput_AdvancesPerTurn(t *testing.T) {
	var mu sync.Mutex
	var perTurn [][]TResponseInputItem
	tool := NewFunctionTool("probe", "probe",
		func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
			mu.Lock()
			perTurn = append(perTurn, tc.TurnInput())
			mu.Unlock()
			return "TOOL_RESULT", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(functionCallOutput(t, "probe", "c2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := Run(context.Background(), agent, "start", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(perTurn) != 2 {
		t.Fatalf("tool ran %d times, want 2", len(perTurn))
	}
	if itemsContain(perTurn[0], "TOOL_RESULT") {
		t.Error("first turn's input must not contain the tool result it has not produced yet")
	}
	if !itemsContain(perTurn[1], "TOOL_RESULT") {
		t.Error("second turn's input should contain the first turn's tool output")
	}
	if len(perTurn[1]) <= len(perTurn[0]) {
		t.Errorf("turn inputs did not grow: %d then %d", len(perTurn[0]), len(perTurn[1]))
	}
}

// The returned slice is a copy: appending to it cannot grow or corrupt the
// run's own view of the turn input.
func TestTurnInput_ReturnsACopy(t *testing.T) {
	var firstLen, secondLen int
	tool := NewFunctionTool("probe", "probe",
		func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
			got := tc.TurnInput()
			firstLen = len(got)
			_ = append(got, InputItemsFromText("INJECTED")...)
			again := tc.TurnInput()
			secondLen = len(again)
			if itemsContain(again, "INJECTED") {
				t.Error("appending to the returned slice leaked into the run's turn input")
			}
			return "ok", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := Run(context.Background(), agent, "hello", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if firstLen == 0 || firstLen != secondLen {
		t.Errorf("turn input length changed across reads: %d then %d", firstLen, secondLen)
	}
}

// A nil RunContext must not panic — hooks and tools may hold one in tests.
func TestTurnInput_NilContext(t *testing.T) {
	var rc *RunContext
	if got := rc.TurnInput(); got != nil {
		t.Errorf("nil context TurnInput = %v, want nil", got)
	}
	rc.setTurnInput([]TResponseInputItem{}) // must not panic
}

func TestTurnInput_MarshalsCleanly(t *testing.T) {
	// Guard against a TurnInput item that cannot round-trip, which would make
	// the accessor useless for callers that log or hash it.
	items := InputItemsFromText("hi")
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "hi") {
		t.Errorf("marshaled turn input = %s", b)
	}
}
