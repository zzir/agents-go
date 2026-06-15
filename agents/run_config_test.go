package agents

import (
	"context"
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
	items := make([]TResponseOutputItem, n)
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

	_, err := Run(context.Background(), agent, "hello", RunOptions{
		Model: model,
		CallModelInputFilter: func(_ context.Context, _ *RunContext, _ *Agent, d ModelInputData) (ModelInputData, error) {
			sawInstr = d.Instructions
			sawLen = len(d.Input)
			d.Instructions = "edited"
			d.Input = append(d.Input, userMsg("injected"))
			return d, nil
		},
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
	var inflight, peak int32
	var mu sync.Mutex
	slow := NewFunctionTool("slow", "slow tool",
		func(_ context.Context, _ *ToolContext, _ struct{}) (string, error) {
			n := atomic.AddInt32(&inflight, 1)
			mu.Lock()
			if n > peak {
				peak = n
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			return "ok", nil
		})
	agent := &Agent{Name: "a", Tools: []Tool{slow}}
	model := &fakeModel{responses: []*ModelResponse{toolCalls(t, "slow", 3), {ResponseID: "done"}}}

	_, err := Run(context.Background(), agent, "go", RunOptions{Model: model, MaxToolConcurrency: 1})
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
	_, err := Run(context.Background(), agent, "go", RunOptions{Model: model})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want tool-not-found abort", err)
	}
}

func TestToolNotFound_ReturnToModelContinues(t *testing.T) {
	agent := &Agent{Name: "a"}
	model := &fakeModel{responses: []*ModelResponse{toolCalls(t, "ghost", 1), {ResponseID: "recovered"}}}
	res, err := Run(context.Background(), agent, "go", RunOptions{
		Model:                model,
		ToolNotFoundBehavior: ToolNotFoundReturnToModel,
	})
	if err != nil {
		t.Fatalf("err = %v, want recovery", err)
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2 (error fed back, model retried)", model.calls)
	}
	var foundErr bool
	for _, it := range res.NewItems {
		if out, ok := it.(*ToolCallOutputItem); ok && strings.Contains(stringifyToolOutput(out.Output), "not found") {
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

	_, err := Run(context.Background(), starter, "go", RunOptions{
		Model: model,
		HandoffInputFilter: func(d HandoffInputData) HandoffInputData {
			called++
			return d
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("run-level handoff filter called %d times, want 1", called)
	}
}
