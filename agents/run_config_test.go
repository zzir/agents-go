package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// toolCalls builds a model response that calls name n times (distinct call IDs).
func toolCalls(t *testing.T, name string, n int) *ModelResponse {
	t.Helper()
	items := make([]OutputItem, n)
	for i := range n {
		items[i] = functionCallOutput(t, name, fmt.Sprintf("call_%d", i), "{}")
	}
	return &ModelResponse{Output: items}
}

func TestCallModelInputFilter_EditsInputAndInstructions(t *testing.T) {
	var sawInstr string
	var sawLen int
	agent := &Agent{Name: "a", Instructions: StaticInstructions("original")}
	model := &fakeModel{responses: []*ModelResponse{{ResponseID: "r1"}}}

	_, err := RunSync(context.Background(), agent, "hello", RunOptions{Model: ModelOptions{Override: model, InputFilter: func(_ context.Context, _ *RunContext, _ *Agent, d ModelInputData) (ModelInputData, error) {
		sawInstr = d.Instructions
		sawLen = len(d.Input)
		d.Instructions = "edited"
		d.Input = append(d.Input, userMsg("injected"))
		return d, nil
	}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawInstr != "original" {
		t.Errorf("filter saw instructions %q, want original", sawInstr)
	}
	if sawLen == 0 {
		t.Error("filter saw empty input")
	}
	if model.lastReq.SystemInstructions != "edited" {
		t.Errorf("model got instructions %q, want edited", model.lastReq.SystemInstructions)
	}
	if got := len(model.lastReq.Input); got != sawLen+1 {
		t.Errorf("model got %d input items, want %d (one injected)", got, sawLen+1)
	}
}

func TestMaxToolConcurrency_LimitsParallelism(t *testing.T) {
	var inflight atomic.Int32
	var peak int32
	var mu sync.Mutex
	slow := NewTool("slow", "slow tool",
		func(_ context.Context, _ *ToolContext, _ struct{}) (string, error) {
			n := inflight.Add(1)
			mu.Lock()
			if n > peak {
				peak = n
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			inflight.Add(-1)
			return "ok", nil
		})
	agent := &Agent{Name: "a", Tools: []*Tool{slow}}
	model := &fakeModel{responses: []*ModelResponse{toolCalls(t, "slow", 3), {ResponseID: "done"}}}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{Exec: ExecOptions{MaxToolConcurrency: 1}, Model: ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}
	if peak > 1 {
		t.Errorf("peak concurrency = %d, want <= 1", peak)
	}
}

func TestToolNotFound_DefaultAborts(t *testing.T) {
	agent := &Agent{Name: "a"}
	model := &fakeModel{responses: []*ModelResponse{toolCalls(t, "ghost", 1)}}
	_, err := RunSync(context.Background(), agent, "go", RunOptions{Model: ModelOptions{Override: model}})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want tool-not-found abort", err)
	}
}

func TestToolNotFound_ReturnToModelContinues(t *testing.T) {
	agent := &Agent{Name: "a"}
	model := &fakeModel{responses: []*ModelResponse{toolCalls(t, "ghost", 1), {ResponseID: "recovered"}}}
	res, err := RunSync(context.Background(), agent, "go", RunOptions{Exec: ExecOptions{ToolNotFoundBehavior: ToolNotFoundReturnToModel}, Model: ModelOptions{Override: model}})
	if err != nil {
		t.Fatalf("err = %v, want recovery", err)
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2 (error fed back, model retried)", model.calls)
	}
	var foundErr bool
	for _, it := range res.NewItems {
		if it.Kind == ItemToolCallOutput && strings.Contains(stringifyToolOutput(it.Output), "not found") {
			foundErr = true
		}
	}
	if !foundErr {
		t.Error("no synthetic tool-not-found output item recorded")
	}
}

func TestHandoffInputFilter_RunLevelDefault(t *testing.T) {
	var called int
	target := &Agent{Name: "target"}
	starter := &Agent{Name: "starter", Handoffs: []Handoff{HandoffTo(target)}}
	model := &fakeModel{responses: []*ModelResponse{
		toolCalls(t, "transfer_to_target", 1), // starter hands off
		{ResponseID: "final"},                 // target finishes
	}}

	_, err := RunSync(context.Background(), starter, "go", RunOptions{Exec: ExecOptions{HandoffInputFilter: func(d HandoffInputData) HandoffInputData {
		called++
		return d
	}}, Model: ModelOptions{Override: model},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("run-level handoff filter called %d times, want 1", called)
	}
}

// reasoningOutput builds a reasoning output item carrying the given id.
func reasoningOutput(t *testing.T, id string) OutputItem {
	t.Helper()
	raw := `{"type":"reasoning","id":` + quote(id) +
		`,"summary":[{"type":"summary_text","text":"think"}]}`
	return mustOutputItem(t, raw)
}

// reasoningInputID returns the id of the first reasoning item in a model input
// list, and whether one was present.
func reasoningInputID(items []InputItem) (string, bool) {
	for i := range items {
		if r := items[i].OfReasoning; r != nil {
			return r.ID, true
		}
	}
	return "", false
}

// With ReasoningItemIDOmit the reasoning id is stripped from the input on
// later turns; the default preserves it.
func TestReasoningItemIDPolicy_Omit(t *testing.T) {
	build := func() (*fakeModel, *Agent) {
		tool := NewTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
			return "ok", nil
		})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(reasoningOutput(t, "rs_1"), functionCallOutput(t, "noop", "c1", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}
		return model, &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
	}

	// Default (preserve): the reasoning id reaches the model on turn 2.
	model, agent := build()
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	id, ok := reasoningInputID(model.lastReq.Input)
	if !ok {
		t.Fatal("preserve: no reasoning item in turn-2 input")
	}
	if id != "rs_1" {
		t.Errorf("preserve: reasoning id = %q, want rs_1", id)
	}

	// Omit: the reasoning id is stripped.
	model, agent = build()
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Exec: ExecOptions{ReasoningItemIDPolicy: ReasoningItemIDOmit}}); err != nil {
		t.Fatal(err)
	}
	id, ok = reasoningInputID(model.lastReq.Input)
	if !ok {
		t.Fatal("omit: no reasoning item in turn-2 input")
	}
	if id != "" {
		t.Errorf("omit: reasoning id = %q, want empty", id)
	}
}

// The policy round-trips through RunState serialization, and states written
// before the field existed default to preserve.
func TestReasoningItemIDPolicy_RunStatePersistence(t *testing.T) {
	agent := &Agent{Name: "a"}
	registry := map[string]*Agent{"a": agent}

	st := &RunState{
		CurrentAgent:          agent,
		Usage:                 NewUsage(),
		ReasoningItemIDPolicy: ReasoningItemIDOmit,
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RunStateFromJSON(data, registry)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningItemIDPolicy != ReasoningItemIDOmit {
		t.Errorf("round-trip policy = %v, want Omit", got.ReasoningItemIDPolicy)
	}

	// A state at the current schema version but without the reasoning-item-id
	// field (an older writer) decodes to the default.
	old := `{"schema_version":"` + RunStateSchemaVersion + `","current_agent":"a","current_turn":1,` +
		`"original_input":[],"generated_items":[],"model_responses":[],` +
		`"interrupted_response":null,"interruptions":[]}`
	got, err = RunStateFromJSON([]byte(old), registry)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningItemIDPolicy != ReasoningItemIDPreserve {
		t.Errorf("legacy state policy = %v, want Preserve", got.ReasoningItemIDPolicy)
	}
}
