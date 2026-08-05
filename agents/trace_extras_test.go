package agents

import (
	"context"
	"errors"
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

// A resume is traced exactly like the run it continues: it starts its own root
// trace, but with the caller's group id and metadata on it. Without them the
// pause and the resume land in different groups — the two halves come apart in
// the very view built to follow one conversation.
func TestResumeRun_TraceGroupIDAndMetadata(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.State.Approve(res.Interruptions[0], false)

	proc := &traceRecordingProcessor{}
	opts := RunOptions{Observe: ObserveOptions{
		Tracer:        tracing.NewTracer(proc),
		TraceGroupID:  "thread-42",
		TraceMetadata: map[string]any{"tenant": "acme"},
	}}
	if _, err := ResumeRunSync(context.Background(), res.State, opts); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("approved tool did not run")
	}
	if len(proc.traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(proc.traces))
	}
	tr := proc.traces[0]
	if tr.GroupID != "thread-42" {
		t.Errorf("GroupID = %q, want thread-42", tr.GroupID)
	}
	if tr.Metadata["tenant"] != "acme" {
		t.Errorf("Metadata = %v, want tenant=acme", tr.Metadata)
	}
	if !strings.HasSuffix(tr.WorkflowName, "(resumed)") {
		t.Errorf("WorkflowName = %q, want the resumed suffix", tr.WorkflowName)
	}
}

// A hand-built state without an agent is a user error, not a panic — and the
// panic used to need a Tracer to show up, so that is what this configures.
func TestResumeRun_NilCurrentAgentIsAUserError(t *testing.T) {
	_, err := ResumeRunSync(context.Background(), &RunState{},
		RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(&traceRecordingProcessor{})}})
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want a *UserError", err, err)
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
		Tools: []*Tool{tool},
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
