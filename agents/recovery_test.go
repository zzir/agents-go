package agents

import (
	"context"
	"strings"
	"testing"
)

// crashedSession builds a session holding a tool call whose output was never
// recorded — what a process killed mid-turn leaves behind.
func crashedSession(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	st := NewInMemoryStorage("crashed")
	sess := NewSession(st)

	user, err := NewItemEntries(InputItemsFromText("send the report"), Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	call, err := EntryFromRunItem(&ToolCallItem{Raw: functionCallOutput(t, "send_email", "c1", `{}`)}, "resp_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, append(user, call)...); err != nil {
		t.Fatal(err)
	}
	return sess
}

// A dangling call is not merely untidy: the Responses API rejects a history
// containing a function_call with no output, so the session is unloadable until
// one exists.
func TestRecovery_RepairsADanglingCall(t *testing.T) {
	ctx := context.Background()
	sess := crashedSession(t)

	// Before: the history cannot be replayed.
	if state := mustState(t, sess); len(state.PendingCallIDs) != 1 {
		t.Fatalf("pending = %v, want the dangling call", state.PendingCallIDs)
	}

	report, err := RecoverSession(ctx, sess, RecoveryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.NeedsRecovery() || len(report.Repaired) != 1 {
		t.Fatalf("report = %+v, want one repaired call", report)
	}
	if state := mustState(t, sess); len(state.PendingCallIDs) != 0 {
		t.Errorf("pending after recovery = %v, want none", state.PendingCallIDs)
	}

	// The synthesized output tells the model what happened, rather than
	// leaving a blank it would read as "the tool returned nothing".
	entries, err := sess.ContextEntries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if !strings.Contains(string(last.Item), "send_email") {
		t.Errorf("the repair does not name the interrupted tool: %s", last.Item)
	}
	if !strings.Contains(string(last.Item), "Do not assume it succeeded") {
		t.Errorf("the repair does not warn against assuming success: %s", last.Item)
	}
	if last.Source.Type != SourceErrorHandler {
		t.Errorf("source = %q, want the repair attributed to the SDK", last.Source.Type)
	}
}

// Nothing is rewritten: the record of what actually happened stays intact.
func TestRecovery_OnlyAppends(t *testing.T) {
	ctx := context.Background()
	sess := crashedSession(t)
	before, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverSession(ctx, sess, RecoveryPolicy{}); err != nil {
		t.Fatal(err)
	}
	after, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("entries went %d → %d, want exactly one appended", len(before), len(after))
	}
	for i := range before {
		if after[i].ID != before[i].ID {
			t.Errorf("entry %d changed identity; recovery must only append", i)
		}
	}
}

// The SDK cannot know whether the crashed call already sent the email, so the
// default is that it did not run again.
func TestRecovery_DoesNotRetryByDefault(t *testing.T) {
	ctx := context.Background()
	report, err := RecoverSession(ctx, crashedSession(t), RecoveryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Retryable) != 0 {
		t.Errorf("retryable = %v, want none without a tool saying it is safe", report.Retryable)
	}
}

// A tool that says it is safe to repeat is left for the next run instead.
func TestRecovery_RetrySafeToolIsLeftDangling(t *testing.T) {
	ctx := context.Background()
	sess := crashedSession(t)
	tools := []Tool{
		WithRetrySafe(NewFunctionTool("send_email", "", func(context.Context, *ToolContext, struct{}) (string, error) {
			return "", nil
		})),
	}

	report, err := RecoverSession(ctx, sess, RecoveryPolicy{RetrySafe: RetrySafeNames(tools)})
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
	tools := []Tool{NewFunctionTool("send_email", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "", nil
	})}
	if RetrySafeNames(tools)("send_email") {
		t.Error("a plain tool reported itself retry-safe")
	}
}

// A healthy session is left completely alone.
func TestRecovery_NoOpOnAHealthySession(t *testing.T) {
	ctx := context.Background()
	sess := NewSession(NewInMemoryStorage("ok"))
	entries, err := NewItemEntries(InputItemsFromText("hello"), Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, entries...); err != nil {
		t.Fatal(err)
	}
	report, err := RecoverSession(ctx, sess, RecoveryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if report.NeedsRecovery() {
		t.Errorf("report = %+v, want nothing to do", report)
	}
	after, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(entries) {
		t.Error("a healthy session was modified")
	}
}

// The repaired history is one the model can actually be sent.
func TestRecovery_RepairedSessionRunsAgain(t *testing.T) {
	ctx := context.Background()
	sess := crashedSession(t)
	if _, err := RecoverSession(ctx, sess, RecoveryPolicy{}); err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "carrying on"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	res, err := RunSync(ctx, agent, "and then?", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatalf("the repaired session could not be run: %v", err)
	}
	if res.FinalOutputString() != "carrying on" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func mustState(t *testing.T, sess *Session) DerivedState {
	t.Helper()
	st, err := sess.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}
