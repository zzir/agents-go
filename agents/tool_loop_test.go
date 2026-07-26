package agents

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func failingTool(t *testing.T, name string) *FunctionTool {
	t.Helper()
	return NewFunctionTool(name, "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "", errors.New("still broken")
	})
}

// A model stuck calling a broken tool must not burn the whole turn budget
// rediscovering that it is broken — and bill for it.
func TestToolLoop_AbortsOnConsecutiveAllFailedTurns(t *testing.T) {
	responses := make([]*ModelResponse, 8)
	for i := range responses {
		responses[i] = modelResp(functionCallOutput(t, "broken", "c1", `{}`))
	}
	model := &fakeModel{responses: responses}
	agent := &Agent{Name: "a", Tools: []Tool{failingTool(t, "broken")}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{MaxTurns: 20, ToolLoop: ToolLoopPolicy{MaxConsecutiveErrorTurns: 3}},
	})
	var tle *ToolLoopError
	if !errors.As(err, &tle) {
		t.Fatalf("err = %v, want *ToolLoopError", err)
	}
	if tle.Turns != 3 {
		t.Errorf("turns = %d, want 3", tle.Turns)
	}
	if CodeOf(err) != CodeToolLoop {
		t.Errorf("code = %q, want %q", CodeOf(err), CodeToolLoop)
	}
	if model.calls != 3 {
		t.Errorf("model calls = %d, want 3 — the valve should stop the run, not the turn budget", model.calls)
	}
}

// One success clears the counter: a tool that fails intermittently is not a
// loop.
func TestToolLoop_ASuccessClearsTheCounter(t *testing.T) {
	var n atomic.Int32
	flaky := NewFunctionTool("flaky", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		if n.Add(1)%2 == 0 {
			return "worked", nil
		}
		return "", errors.New("failed")
	})
	responses := make([]*ModelResponse, 7)
	for i := range responses[:6] {
		responses[i] = modelResp(functionCallOutput(t, "flaky", "c1", `{}`))
	}
	responses[6] = modelResp(messageOutput(t, "done"))
	agent := &Agent{Name: "a", Tools: []Tool{flaky}, ModelImpl: &fakeModel{responses: responses}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{MaxTurns: 20, ToolLoop: ToolLoopPolicy{MaxConsecutiveErrorTurns: 2}},
	})
	if err != nil {
		t.Fatalf("an alternating tool tripped the loop valve: %v", err)
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// A turn with no tool calls is the run talking, not looping.
func TestToolLoop_ATurnWithoutToolsIsNotCounted(t *testing.T) {
	agent := &Agent{Name: "a", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "just talking")),
	}}}
	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{ToolLoop: ToolLoopPolicy{MaxConsecutiveErrorTurns: 1}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestToolLoop_NegativeLimitDisablesTheValve(t *testing.T) {
	responses := make([]*ModelResponse, 4)
	for i := range responses[:3] {
		responses[i] = modelResp(functionCallOutput(t, "broken", "c1", `{}`))
	}
	responses[3] = modelResp(messageOutput(t, "gave up"))
	agent := &Agent{Name: "a", Tools: []Tool{failingTool(t, "broken")}, ModelImpl: &fakeModel{responses: responses}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{MaxTurns: 10, ToolLoop: ToolLoopPolicy{MaxConsecutiveErrorTurns: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "gave up" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// An exhausted budget can end in prose instead of an error — but the extra turn
// must be tool-free, or the model calls something and the budget is gone again
// with nothing said.
func TestToolLoop_FinalTurnWithoutTools(t *testing.T) {
	responses := []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(functionCallOutput(t, "probe", "c2", `{}`)),
		modelResp(messageOutput(t, "here is what I found")),
	}
	tool := NewFunctionTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: responses}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{MaxTurns: 2, ToolLoop: ToolLoopPolicy{FinalTurnWithoutTools: true}},
	})
	if err != nil {
		t.Fatalf("err = %v, want the model to close out in prose", err)
	}
	if res.FinalOutputString() != "here is what I found" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if len(model.lastReq.Tools) != 0 {
		t.Errorf("the final turn offered %d tools, want none", len(model.lastReq.Tools))
	}
}

// Off by default: an exhausted budget is an error, because the budget may be a
// cost ceiling rather than a loop guard.
func TestToolLoop_FinalTurnIsOptIn(t *testing.T) {
	tool := NewFunctionTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(functionCallOutput(t, "probe", "c2", `{}`)),
	}}}
	_, err := RunSync(context.Background(), agent, "go", RunOptions{Exec: ExecOptions{MaxTurns: 2}})
	if !errors.Is(err, ErrMaxTurns) {
		t.Errorf("err = %v, want ErrMaxTurns", err)
	}
}

// A truncated response looks ordinary — items present, no error — but its tail
// may be half-formed. Running a tool call whose arguments stop mid-JSON is how
// an agent acts on something nobody asked for.
func TestTruncatedResponse_ToolCallsDoNotRun(t *testing.T) {
	var ran atomic.Bool
	tool := NewFunctionTool("erase", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		ran.Store(true)
		return "erased", nil
	})
	truncated := modelResp(functionCallOutput(t, "erase", "c1", `{"path": "/ho`))
	truncated.Status = "incomplete"
	truncated.IncompleteReason = "max_output_tokens"
	model := &fakeModel{responses: []*ModelResponse{truncated, modelResp(messageOutput(t, "resent"))}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Error("a tool call from a truncated response executed")
	}
	if res.FinalOutputString() != "resent" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// The model is told what happened, so it can resend rather than being shown
	// a plausible-looking result.
	out := findToolOutput(res.NewItems)
	if out == nil || !strings.Contains(stringifyToolOutput(out.Output), "truncated") {
		t.Errorf("tool output = %v, want an explanation the model can act on", out)
	}
	if out != nil && !out.IsError {
		t.Error("the substituted result is not marked an error")
	}
}

func TestModelResponse_Truncated(t *testing.T) {
	for _, tc := range []struct {
		status, reason string
		want           bool
	}{
		{"completed", "", false},
		{"incomplete", "max_output_tokens", true},
		{"incomplete", "content_filter", false},
		{"", "", false},
	} {
		got := (&ModelResponse{Status: tc.status, IncompleteReason: tc.reason}).Truncated()
		if got != tc.want {
			t.Errorf("Truncated(%q,%q) = %v, want %v", tc.status, tc.reason, got, tc.want)
		}
	}
}

// One sequential tool makes the whole batch sequential — a tool that refuses to
// run beside anything usually means it for a resource the others touch too.
func TestSequentialTool_SerializesTheWholeBatch(t *testing.T) {
	var live, peak atomic.Int32
	body := func(context.Context, *ToolContext, struct{}) (string, error) {
		n := live.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		live.Add(-1)
		return "ok", nil
	}
	serial := WithSequential(NewFunctionTool("serial", "", body))
	parallelA := NewFunctionTool("par_a", "", body)
	parallelB := NewFunctionTool("par_b", "", body)

	model := &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{
			functionCallOutput(t, "serial", "c1", `{}`),
			functionCallOutput(t, "par_a", "c2", `{}`),
			functionCallOutput(t, "par_b", "c3", `{}`),
		}, Usage: NewUsage()},
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{serial, parallelA, parallelB}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 1 {
		t.Errorf("peak concurrency = %d, want 1 — one sequential tool serializes the batch", peak.Load())
	}
}

// Without a sequential tool the batch still runs concurrently.
func TestSequentialTool_AbsentMeansParallel(t *testing.T) {
	var live, peak atomic.Int32
	body := func(context.Context, *ToolContext, struct{}) (string, error) {
		n := live.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		live.Add(-1)
		return "ok", nil
	}
	model := &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{
			functionCallOutput(t, "a", "c1", `{}`),
			functionCallOutput(t, "b", "c2", `{}`),
		}, Usage: NewUsage()},
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "x", ModelImpl: model, Tools: []Tool{
		NewFunctionTool("a", "", body), NewFunctionTool("b", "", body),
	}}
	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 {
		t.Errorf("peak concurrency = %d, want 2 — an ordinary batch runs in parallel", peak.Load())
	}
}
