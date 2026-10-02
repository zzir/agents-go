package agents

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// fakeCompactionSession is an InMemorySession that also records RunCompaction calls.
type fakeCompactionSession struct {
	*session.InMemoryStorage
	calls []session.CompactionArgs
}

func (s *fakeCompactionSession) RunCompaction(_ context.Context, args session.CompactionArgs) error {
	s.calls = append(s.calls, args)
	return nil
}

func TestRunnerInvokesCompaction(t *testing.T) {
	sess := &fakeCompactionSession{InMemoryStorage: session.NewInMemoryStorage("test")}
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []OutputItem{messageOutput(t, "hi")}, Usage: NewUsage(), ResponseID: "resp_42"},
	}}
	agent := &Agent{Name: "a", Model: "m"}

	_, err := RunSync(context.Background(), agent, "hello", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}, Model: ModelOptions{Override: model}})
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
	items, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	if len(items) == 0 {
		t.Error("expected persisted items in the underlying session")
	}
}

// When the model returns no response ID, compaction is still invoked — the
// session decides whether to act (e.g. SlidingWindowStorage ignores ResponseID).
func TestRunnerInvokesCompactionWithoutResponseID(t *testing.T) {
	sess := &fakeCompactionSession{InMemoryStorage: session.NewInMemoryStorage("test")}
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []OutputItem{messageOutput(t, "hi")}, Usage: NewUsage()}, // no ResponseID
	}}
	agent := &Agent{Name: "a", Model: "m"}

	if _, err := RunSync(context.Background(), agent, "hello", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}, Model: ModelOptions{Override: model}}); err != nil {
		t.Fatal(err)
	}
	if len(sess.calls) != 1 {
		t.Fatalf("RunCompaction calls = %d, want 1", len(sess.calls))
	}
	if sess.calls[0].ResponseID != "" {
		t.Errorf("compaction ResponseID = %q, want empty", sess.calls[0].ResponseID)
	}
}
