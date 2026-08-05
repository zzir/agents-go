package agents

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zzir/agents-go/tracing"
)

// traceRecordingProcessor also records trace starts, which recordingProcessor
// ignores.
type traceRecordingProcessor struct {
	recordingProcessor
	tmu    sync.Mutex
	traces []*tracing.Trace
}

func (p *traceRecordingProcessor) OnTraceStart(tr *tracing.Trace) {
	p.tmu.Lock()
	defer p.tmu.Unlock()
	p.traces = append(p.traces, tr)
}

func TestRun_TraceGroupIDAndMetadata(t *testing.T) {
	agent := &Agent{
		Name:      "grouped",
		Model:     "fake-model",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}},
	}
	proc := &traceRecordingProcessor{}
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc), TraceGroupID: "thread-42", TraceMetadata: map[string]any{"tenant": "acme"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(proc.traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(proc.traces))
	}
	tr := proc.traces[0]
	if tr.GroupID != "thread-42" {
		t.Fatalf("GroupID = %q, want thread-42", tr.GroupID)
	}
	if tr.Metadata["tenant"] != "acme" {
		t.Fatalf("Metadata = %v, want tenant=acme", tr.Metadata)
	}
}

func TestNestedAgentToolSpanParentedUnderFunctionSpan(t *testing.T) {
	sub := &Agent{
		Name:      "sub",
		Model:     "fake-model",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "sub done"))}},
	}
	tool := sub.AsTool(AgentToolConfig{Name: "ask_sub"})
	parent := &Agent{
		Name:  "parent",
		Model: "fake-model",
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "ask_sub", "call-1", `{"input":"hi"}`)),
			modelResp(messageOutput(t, "parent done")),
		}},
		Tools: []*FunctionTool{tool},
	}
	proc := &recordingProcessor{}
	if _, err := RunSync(context.Background(), parent, "go", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
		t.Fatal(err)
	}

	var fnSpanID string
	for _, s := range proc.spansOfType(tracing.SpanTypeFunction) {
		if s.Data["name"] == "ask_sub" {
			fnSpanID = s.SpanID
		}
	}
	if fnSpanID == "" {
		t.Fatal("no function span for ask_sub")
	}
	var subSpan *tracing.Span
	for _, s := range proc.spansOfType(tracing.SpanTypeAgent) {
		if strings.Contains(s.Name, "sub") && !strings.Contains(s.Name, "parent") {
			subSpan = s
		}
	}
	if subSpan == nil {
		t.Fatal("no agent span for nested agent")
	}
	if subSpan.ParentID != fnSpanID {
		t.Fatalf("nested agent span ParentID = %q, want the function span %q that invoked it",
			subSpan.ParentID, fnSpanID)
	}
}
