package agents

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"
)

// flakyModel fails the first n calls, then answers.
type flakyModel struct {
	failures int
	calls    int
	answer   *ModelResponse
}

func (m *flakyModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	m.calls++
	if m.calls <= m.failures {
		return nil, errors.New("upstream unavailable")
	}
	return m.answer, nil
}

func (m *flakyModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.calls++
		if m.calls <= m.failures {
			yield(nil, errors.New("upstream unavailable"))
			return
		}
		ev := completedStreamEvent(m.answer)
		yield(&ev, nil)
	}
}

// The interesting failures are the ones that do NOT fail the run: a run that
// answered after three retries looks identical to one that answered first time.
func TestDiagnostics_RetriesAreRecordedOnASuccessfulRun(t *testing.T) {
	inner := &flakyModel{failures: 2, answer: modelResp(messageOutput(t, "finally"))}
	model := NewRetryModel(inner, RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond})
	agent := &Agent{Name: "a", ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "finally" {
		t.Fatalf("final = %q", res.FinalOutputString())
	}
	retries := 0
	for _, d := range res.Diagnostics {
		if d.Type == DiagModelRetry {
			retries++
			if d.Message == "" {
				t.Error("a retry diagnostic carries no reason")
			}
		}
	}
	if retries != 2 {
		t.Errorf("recorded %d retries, want 2 — a successful run still had a bad time",
			retries)
	}
}

// A primary outage that the fallback absorbed leaves no other trace than
// answers getting slower.
func TestDiagnostics_FallbackIsRecorded(t *testing.T) {
	primary := &flakyModel{failures: 99}
	backup := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "from backup"))}}
	agent := &Agent{Name: "a", ModelImpl: NewFallbackModel(primary, backup)}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "from backup" {
		t.Fatalf("final = %q", res.FinalOutputString())
	}
	found := false
	for _, d := range res.Diagnostics {
		if d.Type == DiagModelFallback {
			found = true
			if d.Details["used_index"] != 1 {
				t.Errorf("used_index = %v, want 1", d.Details["used_index"])
			}
		}
	}
	if !found {
		t.Error("the fallback was not recorded")
	}
}

func TestDiagnostics_ToolPanicIsRecorded(t *testing.T) {
	tool := NewTool("boom", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		panic("nope")
	})
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range res.Diagnostics {
		if d.Type == DiagToolPanic {
			found = true
			if d.Code != CodeToolPanic {
				t.Errorf("code = %q, want %q", d.Code, CodeToolPanic)
			}
			if d.Details["tool"] != "boom" {
				t.Errorf("details = %v, want the tool name", d.Details)
			}
		}
	}
	if !found {
		t.Error("the recovered panic was not recorded")
	}
}

// A session that outlives the log has to be able to answer "why was that
// answer bad".
func TestDiagnostics_LandOnTheSessionEntry(t *testing.T) {
	ctx := context.Background()
	sess := NewSession(NewInMemoryStorage("test"))
	inner := &flakyModel{failures: 1, answer: modelResp(messageOutput(t, "ok"))}
	agent := &Agent{Name: "a", ModelImpl: NewRetryModel(inner, RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond})}

	if _, err := RunSync(ctx, agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range entries {
		total += len(e.Diagnostics)
	}
	if total != 1 {
		t.Errorf("%d diagnostics on the session, want the one retry", total)
	}
}

// Each diagnostic belongs to the turn it happened in, not to every turn after.
func TestDiagnostics_AreNotRepeatedAcrossTurns(t *testing.T) {
	ctx := context.Background()
	sess := NewSession(NewInMemoryStorage("test"))
	tool := NewTool("boom", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		panic("once")
	})
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	if _, err := RunSync(ctx, agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range entries {
		total += len(e.Diagnostics)
	}
	if total != 1 {
		t.Errorf("%d diagnostics stored, want 1 — a diagnostic must not repeat on later turns", total)
	}
}

// A failed run explains itself: the error is the last straw, the diagnostics
// are what led to it.
func TestDiagnostics_OnAFailedRun(t *testing.T) {
	tool := NewTool("boom", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		panic("fatal")
	})
	tool.FailureErrorFunction = nil // make the panic fatal
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
	}}}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	re, ok := errors.AsType[*RunError](err)
	if !ok {
		t.Fatalf("err = %v, want a *RunError carrying partial progress", err)
	}
	found := false
	for _, d := range re.Result.Diagnostics {
		if d.Type == DiagToolPanic {
			found = true
		}
	}
	if !found {
		t.Error("a failed run does not report the trouble that led to it")
	}
}

func TestDiagnostics_SinkIsSafeWhenAbsent(t *testing.T) {
	// No sink on the context: recording must be a no-op, not a panic, so a
	// model decorator used outside a run still works.
	RecordDiagnostic(context.Background(), DiagModelRetry, errors.New("x"), nil)
	if DiagnosticsFrom(context.Background()) != nil {
		t.Error("a bare context reported a sink")
	}
	var nilSink *DiagnosticSink
	nilSink.Record(Diagnostic{})
	if nilSink.All() != nil {
		t.Error("a nil sink returned diagnostics")
	}
}
