package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func simpleAgent(t *testing.T, answer string) *Agent {
	t.Helper()
	return &Agent{Name: "a", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, answer)),
	}}}
}

// Middlewares wrap outermost-first, so the order they are written is the order
// they see the run.
func TestMiddleware_OrderIsOutermostFirst(t *testing.T) {
	var order []string
	record := func(name string) RunMiddleware {
		return RunMiddlewareFunc(func(ctx context.Context, next RunFunc, in RunInput) RunStream {
			order = append(order, "enter:"+name)
			stream := next(ctx, in)
			return func(yield func(StreamEvent, error) bool) {
				for ev, err := range stream {
					if !yield(ev, err) {
						return
					}
				}
				order = append(order, "exit:"+name)
			}
		})
	}

	_, err := RunSync(context.Background(), simpleAgent(t, "hi"), "go", RunOptions{
		Middlewares: []RunMiddleware{record("outer"), record("inner")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"enter:outer", "enter:inner", "exit:inner", "exit:outer"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// A middleware may change what the run receives — the point of handing it a
// mutable RunInput rather than a copy.
func TestMiddleware_CanEditInputAndOptions(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	mw := RunMiddlewareFunc(func(ctx context.Context, next RunFunc, in RunInput) RunStream {
		in.Input = append(in.Input, InputItemsFromSystemText("injected by middleware")...)
		in.Opts.Exec.MaxTurns = 3
		return next(ctx, in)
	})

	res, err := RunSync(context.Background(), agent, "original", RunOptions{
		Middlewares: []RunMiddleware{mw},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "ok" {
		t.Errorf("final = %q", res.FinalOutputString())
	}

	sent, err := MarshalItems(model.lastReq.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), "injected by middleware") {
		t.Errorf("the middleware's edit did not reach the model: %s", sent)
	}
	if !strings.Contains(string(sent), "original") {
		t.Errorf("the middleware's edit replaced the caller's input: %s", sent)
	}
}

// A middleware can stop a run before it starts — the reason `next` is a
// parameter rather than something the loop calls unconditionally.
func TestMiddleware_CanRefuseToRun(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "never"))}}
	blocked := errors.New("policy: not allowed")

	mw := RunMiddlewareFunc(func(context.Context, RunFunc, RunInput) RunStream {
		return func(yield func(StreamEvent, error) bool) { yield(nil, blocked) }
	})

	_, err := RunSync(context.Background(), &Agent{Name: "a", ModelImpl: model}, "go", RunOptions{
		Middlewares: []RunMiddleware{mw},
	})
	if !errors.Is(err, blocked) {
		t.Errorf("err = %v, want the middleware's refusal", err)
	}
	if model.calls != 0 {
		t.Errorf("the model was called %d times despite the refusal", model.calls)
	}
}

// Streaming still works through the chain: every event reaches the consumer,
// and the terminal one is not swallowed.
func TestMiddleware_PassesEventsThrough(t *testing.T) {
	pass := RunMiddlewareFunc(func(ctx context.Context, next RunFunc, in RunInput) RunStream {
		return next(ctx, in)
	})
	tool := NewFunctionTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "out", nil })
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "t", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{
		Middlewares: []RunMiddleware{pass, pass},
	})
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.FinalOutputString() != "done" {
		t.Fatalf("result = %+v", res)
	}
	names := map[string]bool{}
	for _, ev := range events {
		if ie, ok := ev.(*RunItemStreamEvent); ok {
			names[ie.Name] = true
		}
	}
	for _, want := range []string{"tool_called", "tool_output", "message_output_created"} {
		if !names[want] {
			t.Errorf("event %q did not survive the chain", want)
		}
	}
}

// A nil middleware in the slice is skipped rather than panicking — a
// conditionally-built chain should not have to filter itself.
func TestMiddleware_NilIsSkipped(t *testing.T) {
	res, err := RunSync(context.Background(), simpleAgent(t, "fine"), "go", RunOptions{
		Middlewares: []RunMiddleware{nil, nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "fine" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}
