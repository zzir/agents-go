package agents

import (
	"context"
	"testing"
)

// fakeCompactionSession is an InMemorySession that also records RunCompaction calls.
type fakeCompactionSession struct {
	*InMemoryStorage
	calls []CompactionArgs
}

func (s *fakeCompactionSession) RunCompaction(_ context.Context, args CompactionArgs) error {
	s.calls = append(s.calls, args)
	return nil
}

func TestRunnerInvokesCompaction(t *testing.T) {
	sess := &fakeCompactionSession{InMemoryStorage: NewInMemoryStorage("test")}
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, "hi")}, Usage: NewUsage(), ResponseID: "resp_42"},
	}}
	agent := &Agent{Name: "a", Model: "m"}

	_, err := RunSync(context.Background(), agent, "hello", RunOptions{Conversation: ConversationOptions{Session: NewSession(sess)}, Model: ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.calls) != 1 {
		t.Fatalf("RunCompaction calls = %d, want 1", len(sess.calls))
	}
	if sess.calls[0].ResponseID != "resp_42" {
		t.Errorf("compaction ResponseID = %q, want resp_42", sess.calls[0].ResponseID)
	}
	// History was still persisted to the underlying session.
	items, _ := NewSession(sess).ContextItems(context.Background(), Cursor{})
	if len(items) == 0 {
		t.Error("expected persisted items in the underlying session")
	}
}

// When the model returns no response ID, compaction is still invoked — the
// session decides whether to act (e.g. SlidingWindowStorage ignores ResponseID).
func TestRunnerInvokesCompactionWithoutResponseID(t *testing.T) {
	sess := &fakeCompactionSession{InMemoryStorage: NewInMemoryStorage("test")}
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, "hi")}, Usage: NewUsage()}, // no ResponseID
	}}
	agent := &Agent{Name: "a", Model: "m"}

	if _, err := RunSync(context.Background(), agent, "hello", RunOptions{Conversation: ConversationOptions{Session: NewSession(sess)}, Model: ModelOptions{Override: model}}); err != nil {
		t.Fatal(err)
	}
	if len(sess.calls) != 1 {
		t.Fatalf("RunCompaction calls = %d, want 1", len(sess.calls))
	}
	if sess.calls[0].ResponseID != "" {
		t.Errorf("compaction ResponseID = %q, want empty", sess.calls[0].ResponseID)
	}
}

// A run whose last item was produced locally — a tool output kept as the final
// output (StopOnFirstTool) or a synthesized error-handler message — must skip
// compaction: those items postdate the last model response, so compacting from
// its response id would erase them from the stored history (Python parity:
// has_local_tool_outputs deferral).
func TestCompactionSkippedWhenRunEndsWithLocalItems(t *testing.T) {
	t.Run("stop on first tool", func(t *testing.T) {
		sess := &fakeCompactionSession{InMemoryStorage: NewInMemoryStorage("test")}
		tool := NewFunctionTool("compute", "computes",
			func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
				return "the-answer", nil
			})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
		}}
		agent := &Agent{Name: "a", Tools: []Tool{tool}, ToolUseBehavior: StopOnFirstTool{}, ModelImpl: model}

		res, err := RunSync(context.Background(), agent, "go", RunOptions{Conversation: ConversationOptions{Session: NewSession(sess)}})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalOutputString() != "the-answer" {
			t.Fatalf("final = %q", res.FinalOutputString())
		}
		if len(sess.calls) != 0 {
			t.Fatalf("RunCompaction calls = %d, want 0 (final turn ends with a local tool output)", len(sess.calls))
		}
	})

	t.Run("max turns recovery message", func(t *testing.T) {
		sess := &fakeCompactionSession{InMemoryStorage: NewInMemoryStorage("test")}
		tool := NewFunctionTool("loop", "loops",
			func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
				return "again", nil
			})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
			modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
		}}
		agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

		opts := RunOptions{Conversation: ConversationOptions{Session: NewSession(sess)}, Exec: ExecOptions{MaxTurns: 1, ErrorHandlers: RunErrorHandlers{
			MaxTurns: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
				return &RunErrorHandlerResult{FinalOutput: "budget spent"}, nil
			},
		}}}
		if _, err := RunSync(context.Background(), agent, "go", opts); err != nil {
			t.Fatal(err)
		}
		if len(sess.calls) != 0 {
			t.Fatalf("RunCompaction calls = %d, want 0 (synthesized fallback message is off the response chain)", len(sess.calls))
		}
	})
}
