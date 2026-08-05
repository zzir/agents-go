package agents

import (
	"context"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

// Runner-facing halves of the session-layer tests: these exercise the run
// loop against a live session, so they live with the runner while the pure
// entry/recovery tests moved to agents/session.

// A run's entries carry provenance and display, so a reader gets the timeline
// the run produced instead of re-deriving it from the wire item.
func TestRunPersistsEntriesWithProvenanceAndDisplay(t *testing.T) {
	sess := session.NewInMemorySession()
	tool := NewTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "tool out", nil })
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "t", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(sess)},
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := sess.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 4 {
		t.Fatalf("stored %d entries, want the input plus the turn's items", len(entries))
	}

	var sawUser, sawToolOutput bool
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("entry stored without an id: %+v", e)
		}
		if e.CreatedAt.IsZero() {
			t.Errorf("entry stored without a timestamp: %+v", e)
		}
		switch e.Source.Type {
		case SourceUser:
			sawUser = true
		case SourceTool:
			sawToolOutput = true
			if e.Display == nil || e.Display.Output != "tool out" {
				t.Errorf("tool output entry lost its display: %+v", e.Display)
			}
		}
	}
	if !sawUser {
		t.Error("no entry attributed to the user")
	}
	if !sawToolOutput {
		t.Error("no entry attributed to a tool")
	}
}

// A tool that says it is safe to repeat is left for the next run instead.
func TestRecovery_RetrySafeToolIsLeftDangling(t *testing.T) {
	ctx := context.Background()
	sess := crashedSession(t)
	sendEmail := NewTool("send_email", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "", nil
	})
	sendEmail.RetrySafe = true
	tools := []*Tool{sendEmail}

	report, err := session.Recover(ctx, sess, session.RecoveryPolicy{RetrySafe: RetrySafeNames(tools)})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Retryable) != 1 || len(report.Repaired) != 0 {
		t.Fatalf("report = %+v, want the call left for a retry", report)
	}
	if state := mustState(t, sess); len(state.PendingCallIDs) != 1 {
		t.Error("a retry-safe call was repaired instead of left dangling")
	}
}

// A tool that never declared itself safe is not made safe by being in the list.
func TestRecovery_UndeclaredToolIsUnsafe(t *testing.T) {
	tools := []*Tool{NewTool("send_email", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "", nil
	})}
	if RetrySafeNames(tools)("send_email") {
		t.Error("a plain tool reported itself retry-safe")
	}
}

// The repaired history is one the model can actually be sent.
func TestRecovery_RepairedSessionRunsAgain(t *testing.T) {
	ctx := context.Background()
	sess := crashedSession(t)
	if _, err := session.Recover(ctx, sess, session.RecoveryPolicy{}); err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "carrying on"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	res, err := RunSync(ctx, agent, "and then?", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatalf("the repaired sess could not be run: %v", err)
	}
	if res.FinalOutputString() != "carrying on" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// crashedSession builds a session holding a dangling tool call — what a
// process killed mid-turn leaves behind — using only the session package's
// public surface, the way any external harness would.
func crashedSession(t *testing.T) *session.Session {
	t.Helper()
	ctx := context.Background()
	sess := session.NewSession(session.NewInMemoryStorage("crashed"))
	user, err := session.NewItemEntries([]InputItem{
		responses.ResponseInputItemParamOfMessage("send the report", responses.EasyInputMessageRoleUser),
	}, Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	call, err := EntryFromRunItem(NewModelItem(ItemToolCall, nil, functionCallOutput(t, "send_email", "c1", `{}`)), "resp_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, append(user, call)...); err != nil {
		t.Fatal(err)
	}
	return sess
}

// mustState folds the session to its derived state, failing the test on error.
func mustState(t *testing.T, sess *session.Session) session.DerivedState {
	t.Helper()
	state, err := sess.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state
}
