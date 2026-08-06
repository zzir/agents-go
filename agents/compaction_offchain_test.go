package agents_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
)

// offChainRecorder is a storage that records the CompactionArgs each run hands
// it. What a self-compacting storage DOES with OffChainItems is its own
// business (openai.CompactionSession switches away from previous_response_id);
// what the runner owes it is an honest answer, which is what these tests pin.
type offChainRecorder struct {
	session.Storage
	calls []session.CompactionArgs
}

func (s *offChainRecorder) RunCompaction(_ context.Context, args session.CompactionArgs) error {
	s.calls = append(s.calls, args)
	return nil
}

func (s *offChainRecorder) onlyCall(t *testing.T) session.CompactionArgs {
	t.Helper()
	if len(s.calls) != 1 {
		t.Fatalf("RunCompaction calls = %d, want 1", len(s.calls))
	}
	return s.calls[0]
}

// seedHistory appends n plain user entries, oldest first.
func seedHistory(t *testing.T, st session.Storage, n int) {
	t.Helper()
	var items []agents.InputItem
	for i := range n {
		items = append(items, agents.InputItemsFromText(fmt.Sprintf("old-%d", i))...)
	}
	if err := session.NewSession(st).AppendItems(t.Context(), items, agents.Source{Type: agents.SourceUser}); err != nil {
		t.Fatal(err)
	}
}

// A windowed read (Conversation.Settings.Limit) sends the model only the newest
// entries, so everything older is stored and on no request — off the response
// chain, and at the FRONT of the log where the position rule cannot see it. A
// storage that rewrites from that chain would delete those entries having never
// read them.
func TestCompactAfterRun_WindowedReadIsOffTheChain(t *testing.T) {
	run := func(t *testing.T, entries, limit int) session.CompactionArgs {
		t.Helper()
		store := &offChainRecorder{Storage: session.NewInMemoryStorage("t")}
		seedHistory(t, store, entries)
		agent := &agents.Agent{Name: "a", ModelImpl: agentstest.TextModel("answer")}
		if _, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{
			Conversation: agents.ConversationOptions{
				Session:  session.NewSession(store),
				Settings: session.Settings{Limit: limit},
			},
		}); err != nil {
			t.Fatal(err)
		}
		return store.onlyCall(t)
	}

	t.Run("history longer than the window", func(t *testing.T) {
		if got := run(t, 6, 2); !got.OffChainItems {
			t.Error("OffChainItems = false; four entries were never sent and are not on the chain")
		}
	})

	// A window the log never reached leaves nothing behind, and the run says so:
	// the answer comes from the read the run already made, not from the setting.
	// Answering from the setting alone would be permanent — a window is not a
	// condition that clears — and a caller who pinned a chain-based compaction
	// mode would never compact again.
	t.Run("history shorter than the window", func(t *testing.T) {
		if got := run(t, 2, 50); got.OffChainItems {
			t.Error("OffChainItems = true; the window never truncated the read, so the model saw the whole log")
		}
	})

	t.Run("no window", func(t *testing.T) {
		if got := run(t, 6, 0); got.OffChainItems {
			t.Error("OffChainItems = true; an unwindowed run sent its whole history")
		}
	})
}

// A handoff input filter rewrites what the next agent sees, and never what is
// stored. Whatever it dropped therefore lives in the log alone: no later model
// call carries it, so the chain rooted at the run's last response cannot hold
// it — and it sits mid-log, where the position rule cannot see it either.
func TestCompactAfterRun_HandoffFilterDropsItemsOffTheChain(t *testing.T) {
	run := func(t *testing.T, filter func(agents.HandoffInputData) agents.HandoffInputData) session.CompactionArgs {
		t.Helper()
		store := &offChainRecorder{Storage: session.NewInMemoryStorage("t")}
		note := agents.NewTool("note", "notes",
			func(context.Context, *agents.ToolContext, struct{}) (string, error) {
				return "only-in-the-log", nil
			})
		target := &agents.Agent{Name: "target"}
		model := agentstest.NewResponseBuilder().
			FunctionCall("note", "c1", `{}`).
			NewTurn().
			FunctionCall("transfer_to_target", "c2", `{}`).
			NewTurn().
			Text("done").
			Build()
		target.ModelImpl = model
		agent := &agents.Agent{
			Name: "a", ModelImpl: model,
			Tools:    []*agents.Tool{note},
			Handoffs: []agents.Handoff{agents.HandoffTo(target)},
		}
		if _, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{
			Conversation: agents.ConversationOptions{Session: session.NewSession(store)},
			Exec:         agents.ExecOptions{HandoffInputFilter: filter},
		}); err != nil {
			t.Fatal(err)
		}
		agentstest.AssertScriptExhausted(t, model)
		return store.onlyCall(t)
	}

	t.Run("filter drops the pre-handoff conversation", func(t *testing.T) {
		got := run(t, func(agents.HandoffInputData) agents.HandoffInputData {
			return agents.HandoffInputData{InputHistory: agents.InputItemsFromText("start over")}
		})
		if !got.OffChainItems {
			t.Error("OffChainItems = false; the filtered-out tool call reached no later model call")
		}
	})

	// A filter that hands back what it was given still sets the flag: telling
	// the two apart means comparing CONTENT, since a filter that redacts in
	// place leaves the length alone, and a comparison that got it wrong would
	// fail by deleting the original unread.
	t.Run("identity filter", func(t *testing.T) {
		got := run(t, func(d agents.HandoffInputData) agents.HandoffInputData { return d })
		if !got.OffChainItems {
			t.Error("OffChainItems = false; a filter that ran is reported without comparing its output")
		}
	})

	t.Run("no filter", func(t *testing.T) {
		if got := run(t, nil); got.OffChainItems {
			t.Error("OffChainItems = true; an unfiltered handoff leaves the whole conversation on the chain")
		}
	})
}

// A pause splits a run in two, and only the second half reaches the after-run
// compaction — so it has to answer for what the first half did. Neither the
// windowed read nor the handoff filter happens again on resume, and the items
// they left off the chain are still in the log, so the answer travels on the
// RunState rather than being recomputed from options the caller may not repeat.
func TestCompactAfterRun_ResumeAnswersForThePausedRun(t *testing.T) {
	// gatedAgent is the pause: its tool stops the run for approval, so the
	// interrupted half never reaches compaction and the resumed half owns the
	// only call.
	gated := func() *agents.Tool {
		tool := agents.NewTool("gated", "needs approval",
			func(context.Context, *agents.ToolContext, struct{}) (string, error) {
				return "ok", nil
			})
		tool.NeedsApproval = true
		return tool
	}

	// resumeThrough runs opts to its interruption, approves it, and resumes
	// under resumeOpts — returning the CompactionArgs of the resumed run.
	resumeThrough := func(t *testing.T, store *offChainRecorder, agent *agents.Agent, opts, resumeOpts agents.RunOptions) session.CompactionArgs {
		t.Helper()
		res, err := agents.RunSync(t.Context(), agent, "go", opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Interruptions) != 1 {
			t.Fatalf("interruptions = %d, want 1", len(res.Interruptions))
		}
		res.State.Approve(res.Interruptions[0], false)
		if _, err := agents.ResumeRunSync(t.Context(), res.State, resumeOpts); err != nil {
			t.Fatal(err)
		}
		return store.onlyCall(t)
	}

	// ResumeRun reads no history, so a caller who does not repeat
	// Conversation.Settings would otherwise hand the resumed run a session that
	// looks unwindowed — and the entries the paused half never sent would be
	// summarized away unread.
	t.Run("a window the caller did not repeat", func(t *testing.T) {
		store := &offChainRecorder{Storage: session.NewInMemoryStorage("t")}
		seedHistory(t, store, 6)
		agent := &agents.Agent{
			Name:  "a",
			Tools: []*agents.Tool{gated()},
			ModelImpl: agentstest.NewResponseBuilder().
				FunctionCall("gated", "c1", `{}`).
				NewTurn().
				Text("done").
				Build(),
		}
		sess := session.NewSession(store)
		got := resumeThrough(t, store, agent,
			agents.RunOptions{Conversation: agents.ConversationOptions{
				Session: sess, Settings: session.Settings{Limit: 2},
			}},
			agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}})
		if !got.OffChainItems {
			t.Error("OffChainItems = false; the window that truncated the first half's read still applies to its log")
		}
	})

	// A RunState carries the FILTERED input, so the resumed run cannot see that
	// anything was dropped — it has to be told.
	t.Run("a handoff filter that ran before the pause", func(t *testing.T) {
		store := &offChainRecorder{Storage: session.NewInMemoryStorage("t")}
		model := agentstest.NewResponseBuilder().
			FunctionCall("transfer_to_target", "c1", `{}`).
			NewTurn().
			FunctionCall("gated", "c2", `{}`).
			NewTurn().
			Text("done").
			Build()
		target := &agents.Agent{Name: "target", ModelImpl: model, Tools: []*agents.Tool{gated()}}
		agent := &agents.Agent{
			Name: "a", ModelImpl: model,
			Handoffs: []agents.Handoff{agents.HandoffTo(target)},
		}
		opts := agents.RunOptions{
			Conversation: agents.ConversationOptions{Session: session.NewSession(store)},
			Exec: agents.ExecOptions{HandoffInputFilter: func(agents.HandoffInputData) agents.HandoffInputData {
				return agents.HandoffInputData{InputHistory: agents.InputItemsFromText("start over")}
			}},
		}
		got := resumeThrough(t, store, agent, opts, opts)
		agentstest.AssertScriptExhausted(t, model)
		if !got.OffChainItems {
			t.Error("OffChainItems = false; the pre-pause filter dropped a conversation that is still in the log")
		}
	})
}
