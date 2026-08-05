package agents

import (
	"context"
	"strings"
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

// The runner reports whether the run left items the server's response chain
// cannot hold, and lets the storage decide what to do about it. It does not
// decide by skipping: a storage that never looks at a chain has nothing to be
// wrong about, and skipping starved it — an agent that always finishes through
// a terminating tool would never compact at all.
//
// The frontier is POSITION, not provenance. A run resends its whole input every
// turn, so a tool output buried mid-run stood in front of the model when it
// answered and is on the chain; only what came after the last response is not.
func TestCompactAfterRun_ReportsItemsOffTheChain(t *testing.T) {
	t.Run("tool output buried mid-run is on the chain", func(t *testing.T) {
		sess := &fakeCompactionSession{InMemoryStorage: session.NewInMemoryStorage("test")}
		tool := NewTool("compute", "computes",
			func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) { return "42", nil })
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
			modelResp(messageOutput(t, "the answer is 42")),
		}}
		agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

		if _, err := RunSync(context.Background(), agent, "go",
			RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
			t.Fatal(err)
		}
		if len(sess.calls) != 1 {
			t.Fatalf("RunCompaction calls = %d, want 1", len(sess.calls))
		}
		if sess.calls[0].OffChainItems {
			t.Error("OffChainItems = true; the tool output was resent with the next turn's input")
		}
	})

	t.Run("tool terminates the run", func(t *testing.T) {
		sess := &fakeCompactionSession{InMemoryStorage: session.NewInMemoryStorage("test")}
		tool := NewTool("compute", "computes",
			func(ctx context.Context, tc *ToolContext, args struct{}) (ToolResult, error) {
				r := TextResult("the-answer")
				r.Terminate = true
				return r, nil
			})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
		}}
		agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

		res, err := RunSync(context.Background(), agent, "go", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalOutputString() != "the-answer" {
			t.Fatalf("final = %q", res.FinalOutputString())
		}
		if len(sess.calls) != 1 {
			t.Fatalf("RunCompaction calls = %d, want 1 (the storage decides, not the runner)", len(sess.calls))
		}
		if !sess.calls[0].OffChainItems {
			t.Error("OffChainItems = false; the terminating tool's output postdates the last response")
		}
	})

	t.Run("max turns recovery message", func(t *testing.T) {
		sess := &fakeCompactionSession{InMemoryStorage: session.NewInMemoryStorage("test")}
		tool := NewTool("loop", "loops",
			func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
				return "again", nil
			})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
			modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
		}}
		agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

		opts := RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}, Exec: ExecOptions{MaxTurns: 1, ErrorHandlers: RunErrorHandlers{
			MaxTurns: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
				return &RunErrorHandlerResult{FinalOutput: "budget spent"}, nil
			},
		}}}
		if _, err := RunSync(context.Background(), agent, "go", opts); err != nil {
			t.Fatal(err)
		}
		if len(sess.calls) != 1 {
			t.Fatalf("RunCompaction calls = %d, want 1", len(sess.calls))
		}
		if !sess.calls[0].OffChainItems {
			t.Error("OffChainItems = false; the synthesized fallback message is off the response chain")
		}
	})
}

// chainRewritingStorage models the server-side compact API: it REPLACES the log
// with a summary, and the summary can only cover what the response chain holds
// — modeled here as the last model request's input, since a run with a local
// session resends its whole conversation every turn. OffChainItems is the
// runner's warning that the log holds more than that; honoring it means
// summarizing the stored log instead, which is what the input mode of
// openai.CompactionSession does.
type chainRewritingStorage struct {
	session.Storage
	model *fakeModel
	calls int
}

func (s *chainRewritingStorage) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	s.calls++
	var texts []string
	if args.OffChainItems {
		entries, err := s.Entries(ctx, session.Cursor{})
		if err != nil {
			return err
		}
		for _, e := range entries {
			if in, ierr := e.InputItem(); ierr == nil {
				texts = append(texts, session.ItemText(in))
			}
		}
	} else {
		for _, in := range s.model.lastReq.Input {
			texts = append(texts, session.ItemText(in))
		}
	}
	summary, err := session.NewItemEntries(
		InputItemsFromText("SUMMARY: "+strings.Join(texts, " ")), Source{Type: SourceCompaction})
	if err != nil {
		return err
	}
	return session.ReplaceEntries(ctx, s.Storage, summary...)
}

// A steer taken after the last model response is stored, counted delivered, and
// then — before this fix — replaced by a summary written from a chain that
// never saw it. It is external by provenance, so the old last-item-is-local
// guard passed it straight through, and no model call ever carried it, so
// nothing else could have.
func TestCompactAfterRun_ChainRewriteKeepsWhatTheChainNeverSaw(t *testing.T) {
	const steer = "and use metric units"
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "first answer")),
		modelResp(messageOutput(t, "never reached")),
	}}
	store := &chainRewritingStorage{Storage: session.NewInMemoryStorage("test"), model: model}
	agent := &Agent{Name: "a", ModelImpl: model}

	// MaxTurns ends the run on the turn the continuation would have ridden on,
	// so the steer is the last thing in the log and no model call follows it.
	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(store)},
		Exec: ExecOptions{MaxTurns: 1, ErrorHandlers: RunErrorHandlers{
			MaxTurns: func(context.Context, RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
				return &RunErrorHandlerResult{FinalOutput: "budget spent", ExcludeFromHistory: true}, nil
			},
		}},
	})
	steered := false
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if it, ok := ev.(*RunItemStreamEvent); ok && it.Item.Kind == ItemMessage && !steered {
			steered = true
			if serr := ctrl.Steer(steer); serr != nil {
				t.Fatal(serr)
			}
		}
	}
	if !steered {
		t.Fatal("the run produced no message to steer after")
	}
	if store.calls != 1 {
		t.Fatalf("RunCompaction calls = %d, want 1", store.calls)
	}
	// The steer never reached the model, so a summary of the chain cannot
	// mention it — the rewrite would be deleting something nothing ever read.
	for _, in := range model.lastReq.Input {
		if strings.Contains(session.ItemText(in), steer) {
			t.Fatal("test setup: the steer DID reach the model, so the chain holds it")
		}
	}
	items, err := session.NewSession(store).ContextItems(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, in := range items {
		got = append(got, session.ItemText(in))
	}
	if !strings.Contains(strings.Join(got, " "), steer) {
		t.Errorf("the steer was erased by a compaction that never saw it; history = %q", got)
	}
}
