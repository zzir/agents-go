package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// A long tool call has to be watchable, or the only honest thing a UI can show
// is a spinner.
func TestToolProgress_ReachesTheStream(t *testing.T) {
	tool := NewFunctionTool("build", "", func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
		for _, line := range []string{"step 1", "step 2", "step 3"} {
			tc.Emit(TextResult(line))
		}
		return "built", nil
	})
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "build", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{})
	var progress []string
	var final *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		switch e := ev.(type) {
		case *ToolProgressEvent:
			if e.CallID != "c1" || e.ToolName != "build" {
				t.Errorf("progress event does not identify its call: %+v", e)
			}
			progress = append(progress, stringifyToolOutput(e.Result.ModelOutput()))
		case *RunCompletedEvent:
			final = e.Result
		}
	}
	if len(progress) != 3 {
		t.Errorf("got %d progress events (%v), want 3", len(progress), progress)
	}
	// Progress is not the answer: the model gets what the tool returned.
	if final == nil {
		t.Fatal("no result")
	}
	out := findToolOutput(final.NewItems)
	if out == nil || stringifyToolOutput(out.Output) != "built" {
		t.Errorf("tool output = %v, want the returned value, not a partial", out)
	}
}

// Progress is dropped on a blocking run: nobody is watching, and buffering it
// would grow without bound for a consumer that will never read it.
func TestToolProgress_NoOpOnABlockingRun(t *testing.T) {
	emitted := 0
	tool := NewFunctionTool("build", "", func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
		for range 100 {
			tc.Emit(TextResult("noise"))
			emitted++
		}
		return "built", nil
	})
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "build", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if emitted != 100 {
		t.Errorf("Emit blocked the tool: %d calls completed", emitted)
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// A goroutine the tool left running would otherwise keep pushing progress for a
// call that is already answered, and a consumer could not tell that from a call
// still working.
func TestToolProgress_IgnoredAfterTheToolReturns(t *testing.T) {
	var late sync.WaitGroup
	late.Add(1)
	release := make(chan struct{})
	tool := NewFunctionTool("leaky", "", func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
		go func() {
			defer late.Done()
			<-release
			tc.Emit(TextResult("too late"))
		}()
		return "done", nil
	})
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "leaky", "c1", `{}`)),
		modelResp(messageOutput(t, "ok")),
	}}}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{})
	var seen []string
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := ev.(*ToolProgressEvent); ok {
			seen = append(seen, stringifyToolOutput(p.Result.ModelOutput()))
		}
	}
	close(release)
	late.Wait()
	for _, s := range seen {
		if s == "too late" {
			t.Error("progress from a finished call reached the consumer")
		}
	}
}

// Several tools stream at once and the run loop yields too; an iterator's yield
// is not safe for concurrent calls, so this is the test that would catch it.
func TestToolProgress_ConcurrentToolsDoNotRaceTheStream(t *testing.T) {
	mk := func(name string) *FunctionTool {
		return NewFunctionTool(name, "", func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
			for i := range 50 {
				tc.Emit(TextResult(fmt.Sprintf("%s-%d", name, i)))
			}
			return name + " done", nil
		})
	}
	model := &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{
			functionCallOutput(t, "a", "c1", `{}`),
			functionCallOutput(t, "b", "c2", `{}`),
			functionCallOutput(t, "c", "c3", `{}`),
		}, Usage: NewUsage()},
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "x", Tools: []*FunctionTool{mk("a"), mk("b"), mk("c")}, ModelImpl: model}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{})
	counts := map[string]int{}
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := ev.(*ToolProgressEvent); ok {
			counts[p.ToolName]++
		}
	}
	for _, name := range []string{"a", "b", "c"} {
		if counts[name] != 50 {
			t.Errorf("tool %s produced %d progress events, want 50", name, counts[name])
		}
	}
}

// A nested agent's work shows up on the parent's stream without the caller
// wiring OnStream.
func TestToolProgress_NestedAgentReportsThrough(t *testing.T) {
	inner := &Agent{Name: "inner", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "inner thinking")),
	}}}
	outer := &Agent{Name: "outer", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "ask", "c1", `{"input":"hi"}`)),
		modelResp(messageOutput(t, "outer answer")),
	}}}
	outer.Tools = []*FunctionTool{inner.AsTool(AgentToolConfig{Name: "ask"})}

	stream, _ := Run(context.Background(), outer, "go", RunOptions{})
	found := false
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := ev.(*ToolProgressEvent); ok {
			if strings.Contains(stringifyToolOutput(p.Result.ModelOutput()), "inner thinking") {
				found = true
				if p.Result.Details["nested_agent"] != "inner" {
					t.Errorf("details = %v, want the nested agent's name", p.Result.Details)
				}
			}
		}
	}
	if !found {
		t.Error("the nested agent's work did not reach the parent stream")
	}
}
