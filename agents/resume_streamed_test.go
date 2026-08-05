package agents

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// collectItemEvents drains a run stream and returns its RunItemStreamEvent
// names in order, suffixed with the tool call id where the item carries one.
func collectItemEvents(t *testing.T, stream RunStream) ([]string, *RunResult) {
	t.Helper()
	var names []string
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	for _, ev := range events {
		ie, ok := ev.(*RunItemStreamEvent)
		if !ok {
			continue
		}
		name := ie.Name
		switch ie.Item.Kind {
		case ItemToolCall, ItemToolCallOutput:
			if id := ie.Item.CallID(); id != "" {
				name += ":" + id
			}
		}
		names = append(names, name)
	}
	return names, res
}

// A streamed resume must pick up exactly where the interrupted segment left
// off: the paused turn's own items (message, gated tool call) were already
// emitted before the pause and are NOT re-emitted; the resume streams the
// approved tool's output and every later turn's items.
func TestHITL_ResumeRunStreamed_EmitsResumedSegmentEvents(t *testing.T) {
	var wroteFile, ranCommand bool
	gated := NewFunctionTool("write_file", "gated",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			wroteFile = true
			return "written", nil
		})
	gated.NeedsApproval = true
	free := NewFunctionTool("exec_command", "free",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ranCommand = true
			return "executed", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "writing the file"), functionCallOutput(t, "write_file", "call_1", `{}`)),
		modelResp(messageOutput(t, "now running it"), functionCallOutput(t, "exec_command", "call_2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*FunctionTool{gated, free}, ModelImpl: model}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{})
	interrupted, res := collectItemEvents(t, stream)
	if res == nil {
		t.Fatal("stream produced no result")
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	wantInterrupted := []string{"message_output_created", "tool_called:call_1"}
	if !reflect.DeepEqual(interrupted, wantInterrupted) {
		t.Errorf("interrupted segment events = %v, want %v", interrupted, wantInterrupted)
	}
	if wroteFile {
		t.Error("gated tool ran before approval")
	}

	res.State.Approve(res.Interruptions[0], false)
	stream2, _ := ResumeRun(context.Background(), res.State, RunOptions{})
	resumed, res2 := collectItemEvents(t, stream2)
	if res2 == nil {
		t.Fatal("resumed stream produced no result")
	}
	wantResumed := []string{
		"tool_output:call_1",
		"message_output_created",
		"tool_called:call_2",
		"tool_output:call_2",
		"message_output_created",
	}
	if !reflect.DeepEqual(resumed, wantResumed) {
		t.Errorf("resumed segment events = %v, want %v", resumed, wantResumed)
	}
	if !wroteFile || !ranCommand {
		t.Errorf("tools ran: write_file=%v exec_command=%v, want both", wroteFile, ranCommand)
	}
	if res2.FinalOutputString() != "done" {
		t.Errorf("final = %q, want %q", res2.FinalOutputString(), "done")
	}
}

// A resume that interrupts again must surface the new interruption through
// FinalResult, mirroring the blocking ResumeRun.
func TestHITL_ResumeRunStreamed_ReInterrupt(t *testing.T) {
	gated := NewFunctionTool("step", "gated",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	gated.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "step", "call_1", `{}`)),
		modelResp(functionCallOutput(t, "step", "call_2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*FunctionTool{gated}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}

	// Approve only the first call: the resumed run's next turn requests the
	// tool again and must interrupt again.
	res.State.Approve(res.Interruptions[0], false)
	stream, _ := ResumeRun(context.Background(), res.State, RunOptions{})
	events, res2 := collectItemEvents(t, stream)
	if res2 == nil {
		t.Fatal("resumed stream produced no result")
	}
	if len(res2.Interruptions) != 1 {
		t.Fatalf("expected re-interruption, got %d interruptions", len(res2.Interruptions))
	}
	want := []string{"tool_output:call_1", "tool_called:call_2"}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("resumed segment events = %v, want %v", events, want)
	}
	if res2.State == nil {
		t.Fatal("expected RunState on re-interruption")
	}
}

func TestHITL_ResumeRunStreamed_NilState(t *testing.T) {
	stream, _ := ResumeRun(context.Background(), nil, RunOptions{})
	_, _, err := streamRun(stream)
	if err == nil {
		t.Fatal("a nil state must fail the stream")
	}
	if !strings.Contains(err.Error(), "state must not be nil") {
		t.Errorf("err = %v, want it to name the nil state", err)
	}
}
