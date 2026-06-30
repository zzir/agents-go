package agents

import (
	"context"
	"testing"
)

// fakeCompactionSession is an InMemorySession that also records RunCompaction calls.
type fakeCompactionSession struct {
	*InMemorySession
	calls []CompactionArgs
}

func (s *fakeCompactionSession) RunCompaction(_ context.Context, args CompactionArgs) error {
	s.calls = append(s.calls, args)
	return nil
}

func TestRunnerInvokesCompaction(t *testing.T) {
	sess := &fakeCompactionSession{InMemorySession: NewInMemorySession()}
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, "hi")}, Usage: NewUsage(), ResponseID: "resp_42"},
	}}
	agent := &Agent{Name: "a", Model: "m"}

	_, err := Run(context.Background(), agent, "hello", RunOptions{Model: model, Session: sess})
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
	items, _ := sess.GetItems(context.Background(), 0)
	if len(items) == 0 {
		t.Error("expected persisted items in the underlying session")
	}
}

// When the model returns no response ID, compaction is still invoked — the
// session decides whether to act (e.g. SlidingWindowSession ignores ResponseID).
func TestRunnerInvokesCompactionWithoutResponseID(t *testing.T) {
	sess := &fakeCompactionSession{InMemorySession: NewInMemorySession()}
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, "hi")}, Usage: NewUsage()}, // no ResponseID
	}}
	agent := &Agent{Name: "a", Model: "m"}

	if _, err := Run(context.Background(), agent, "hello", RunOptions{Model: model, Session: sess}); err != nil {
		t.Fatal(err)
	}
	if len(sess.calls) != 1 {
		t.Fatalf("RunCompaction calls = %d, want 1", len(sess.calls))
	}
	if sess.calls[0].ResponseID != "" {
		t.Errorf("compaction ResponseID = %q, want empty", sess.calls[0].ResponseID)
	}
}
