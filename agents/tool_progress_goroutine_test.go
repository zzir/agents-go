package agents_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

// A range body that panics on a progress event must unwind the consumer's own
// goroutine with its own value — not be recovered as the tool's failure.
func TestToolProgressBodyPanicReachesTheConsumer(t *testing.T) {
	tool := agents.NewTool("build", "",
		func(_ context.Context, tc *agents.ToolContext, _ struct{}) (string, error) {
			tc.Emit(agents.TextResult("step 1"))
			return "built", nil
		})
	model := agentstest.NewResponseBuilder().
		FunctionCall("build", "c1", "{}").
		NewTurn().
		Text("done").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{tool}}
	stream, _ := agents.Run(context.Background(), agent, "go", agents.RunOptions{})

	got := func() (p any) {
		defer func() { p = recover() }()
		for ev, err := range stream {
			if err != nil {
				return err
			}
			if _, ok := ev.(*agents.ToolProgressEvent); ok {
				panic("consumer bug")
			}
		}
		return nil
	}()
	if got != "consumer bug" {
		t.Fatalf("the range body's panic surfaced as %v, want its own value", got)
	}
}

// A serialized batch launches its second call only after the first returns;
// the first call's progress still has to reach the stream meanwhile.
func TestToolProgressDrainsWhileASequentialBatchLaunches(t *testing.T) {
	tool := agents.NewTool("step", "",
		func(_ context.Context, tc *agents.ToolContext, _ struct{}) (string, error) {
			tc.Emit(agents.TextResult("working"))
			return "ok", nil
		})
	model := agentstest.NewResponseBuilder().
		FunctionCall("step", "c1", "{}").
		FunctionCall("step", "c2", "{}").
		NewTurn().
		Text("done").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{tool}}
	stream, _ := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Exec: agents.ExecOptions{MaxToolConcurrency: 1},
	})

	var progress atomic.Int32
	var runErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for ev, err := range stream {
			if err != nil {
				runErr = err
				return
			}
			if _, ok := ev.(*agents.ToolProgressEvent); ok {
				progress.Add(1)
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish: progress was not drained while the batch launched")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if got := progress.Load(); got != 2 {
		t.Fatalf("got %d progress events, want 2", got)
	}
}
