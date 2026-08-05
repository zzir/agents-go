package agents

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// recordingStorage wraps session.InMemoryStorage and records each Append batch, so
// tests can assert how many times (and with what) the runner persisted.
type recordingSession struct {
	*session.InMemoryStorage
	mu      sync.Mutex
	batches [][]session.Entry
}

func newRecordingSession() *recordingSession {
	return &recordingSession{InMemoryStorage: session.NewInMemoryStorage("test")}
}

func (s *recordingSession) Append(ctx context.Context, entries ...session.Entry) error {
	s.mu.Lock()
	s.batches = append(s.batches, append([]session.Entry(nil), entries...))
	s.mu.Unlock()
	return s.InMemoryStorage.Append(ctx, entries...)
}

func (s *recordingSession) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

// cancelOnCallModel cancels the run's context and returns context.Canceled on
// its `at`-th Respond call, simulating a cancel that arrives after earlier
// turns have already completed and persisted.
type cancelOnCallModel struct {
	*fakeModel
	cancel context.CancelFunc
	at     int
}

func (m *cancelOnCallModel) Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	if m.calls+1 == m.at {
		m.calls++
		m.cancel()
		return nil, context.Canceled
	}
	return m.fakeModel.Respond(ctx, req)
}

// itemStats classifies stored input items for assertions.
type itemStats struct {
	users, assistants, calls, outputs int
	callIDs, outputIDs                map[string]bool
}

func classify(items []InputItem) itemStats {
	st := itemStats{callIDs: map[string]bool{}, outputIDs: map[string]bool{}}
	for i := range items {
		switch {
		case items[i].OfFunctionCall != nil:
			st.calls++
			st.callIDs[items[i].OfFunctionCall.CallID] = true
		case items[i].OfFunctionCallOutput != nil:
			st.outputs++
			st.outputIDs[items[i].OfFunctionCallOutput.CallID] = true
		case items[i].OfMessage != nil:
			// EasyInputMessage: user/system/developer input, or an assistant
			// message rebuilt from serialized state.
			if items[i].OfMessage.Role == "assistant" {
				st.assistants++
			} else {
				st.users++
			}
		case items[i].OfOutputMessage != nil:
			// Assistant output message replayed as input (live run).
			st.assistants++
		}
	}
	return st
}

// A tool that always succeeds, used to drive multi-turn loops.
func echoTool(ran *int) *Tool {
	return NewTool("echo", "echoes", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		if ran != nil {
			*ran++
		}
		return "ok", nil
	})
}

// A successful multi-turn run persists incrementally: user input up front, then
// each turn as it completes — not a single save at the end.
func TestSession_PersistsEachTurn(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "step one"), functionCallOutput(t, "echo", "call_1", `{}`)),
		modelResp(messageOutput(t, "step two"), functionCallOutput(t, "echo", "call_2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{echoTool(nil)}, ModelImpl: model}

	sess := newRecordingSession()
	if _, err := RunSync(context.Background(), agent, "go", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
		t.Fatal(err)
	}

	// user input + two tool turns + final turn = 4 distinct saves (not 1).
	if got := sess.saveCount(); got < 3 {
		t.Errorf("expected incremental saves (>=3), got %d", got)
	}

	items, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	st := classify(items)
	if st.users != 1 {
		t.Errorf("users = %d, want 1", st.users)
	}
	if st.calls != 2 || st.outputs != 2 {
		t.Errorf("calls=%d outputs=%d, want 2/2", st.calls, st.outputs)
	}
	for id := range st.callIDs {
		if !st.outputIDs[id] {
			t.Errorf("call %s has no matching output in stored session", id)
		}
	}
}

// The crux: a run cancelled after turn 1 completes keeps turn 1's message, tool
// call and tool output in the session — instead of losing the whole run.
func TestSession_CancelKeepsCompletedTurns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inner := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "working on it"), functionCallOutput(t, "echo", "call_1", `{}`)),
	}}
	model := &cancelOnCallModel{fakeModel: inner, cancel: cancel, at: 2}
	agent := &Agent{Name: "a", Tools: []*Tool{echoTool(nil)}, ModelImpl: model}

	sess := newRecordingSession()
	_, err := RunSync(ctx, agent, "go", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	items, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	st := classify(items)
	if st.users != 1 {
		t.Errorf("user input lost: users = %d, want 1", st.users)
	}
	if st.assistants != 1 {
		t.Errorf("completed assistant message lost: assistants = %d, want 1", st.assistants)
	}
	if st.calls != 1 || st.outputs != 1 || !st.outputIDs["call_1"] {
		t.Errorf("completed tool call/output lost: calls=%d outputs=%d ids=%v", st.calls, st.outputs, st.outputIDs)
	}
}

// A failed model call before any turn completes still records the user input
// (persisted up front at loop start), so the prompt is not lost.
func TestSession_FailedFirstTurnKeepsUserInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inner := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "unused"))}}
	model := &cancelOnCallModel{fakeModel: inner, cancel: cancel, at: 1}
	agent := &Agent{Name: "a", ModelImpl: model}

	sess := newRecordingSession()
	if _, err := RunSync(ctx, agent, "hello", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	items, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	if st := classify(items); st.users != 1 || st.assistants != 0 {
		t.Errorf("want just the user prompt, got users=%d assistants=%d", st.users, st.assistants)
	}
}

// At an interruption the pending, output-less tool call is held back so the
// session never stores a call without its output; the resumed run persists the
// call together with its output. End state has no dangling call.
func TestSession_InterruptionHoldsBackPendingCall(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	sess := newRecordingSession()

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}

	// Paused: only the prompt is stored — the pending call is withheld.
	paused, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	if st := classify(paused); st.users != 1 || st.calls != 0 || st.outputs != 0 {
		t.Errorf("at pause want user only, got users=%d calls=%d outputs=%d", st.users, st.calls, st.outputs)
	}

	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRunSync(context.Background(), res.State, RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
		t.Fatal(err)
	}

	// Resumed to completion: the call and its output are both present and paired.
	final, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	st := classify(final)
	if st.users != 1 || st.calls != 1 || st.outputs != 1 {
		t.Errorf("after resume want 1/1/1, got users=%d calls=%d outputs=%d", st.users, st.calls, st.outputs)
	}
	if !st.outputIDs["call_1"] {
		t.Errorf("call_1 stored without its output: %v", st.outputIDs)
	}
}

// A resumed run must not re-persist the turns the interrupted run already saved:
// the persistence cursor rides across the interrupt/resume boundary.
func TestSession_ResumeDoesNotDuplicate(t *testing.T) {
	// Turn 1 runs a non-approval tool (persisted), turn 2 needs approval
	// (interrupts), resume finishes.
	safeTool := echoTool(nil)
	danger := NewTool("danger", "needs ok", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		return "boom", nil
	})
	danger.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "first"), functionCallOutput(t, "echo", "call_1", `{}`)),
		modelResp(functionCallOutput(t, "danger", "call_2", `{}`)),
		modelResp(messageOutput(t, "all done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{safeTool, danger}, ModelImpl: model}

	sess := newRecordingSession()
	res, err := RunSync(context.Background(), agent, "go", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected interruption, got %d", len(res.Interruptions))
	}

	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRunSync(context.Background(), res.State, RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
		t.Fatal(err)
	}

	items, _ := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	st := classify(items)
	if st.users != 1 {
		t.Errorf("user duplicated: users = %d, want 1", st.users)
	}
	// call_1 (echo) + call_2 (danger), each with exactly one output — no dupes.
	if st.calls != 2 || st.outputs != 2 {
		t.Errorf("call/output duplicated or lost: calls=%d outputs=%d, want 2/2", st.calls, st.outputs)
	}
	if !st.callIDs["call_1"] || !st.callIDs["call_2"] || !st.outputIDs["call_1"] || !st.outputIDs["call_2"] {
		t.Errorf("missing paired call/output: callIDs=%v outputIDs=%v", st.callIDs, st.outputIDs)
	}
}

// PersistedSessionItems survives RunState JSON round-trips; states written
// before the field existed decode as zero.
func TestRunState_PersistedCursorRoundTrip(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{Conversation: ConversationOptions{Session: session.NewInMemorySession()}})
	if err != nil {
		t.Fatal(err)
	}
	res.State.PersistedSessionItems = 3

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	if restored.PersistedSessionItems != 3 {
		t.Errorf("PersistedSessionItems = %d, want 3", restored.PersistedSessionItems)
	}
}

// session.Settings.Limit caps how many history items are loaded at run start.
func TestSessionSettings_Limit(t *testing.T) {
	sess := session.NewInMemorySession()
	_ = sess.AppendItems(context.Background(), InputItemsFromText("h1"), Source{})
	_ = sess.AppendItems(context.Background(), InputItemsFromText("h2"), Source{})
	_ = sess.AppendItems(context.Background(), InputItemsFromText("h3"), Source{})

	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "new", RunOptions{Conversation: ConversationOptions{Session: sess, Settings: session.Settings{Limit: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	// One history item (the limit) plus the new input.
	if got := len(model.lastReq.Input); got != 2 {
		t.Errorf("turn-1 input length = %d, want 2 (1 history + 1 new)", got)
	}
}

func TestInMemorySession(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "hi there")),
		modelResp(messageOutput(t, "you said hello before")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	// First run.
	if _, err := RunSync(ctx, agent, "hello", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
		t.Fatal(err)
	}
	items, _ := session.NewSession(sess).ContextItems(ctx, session.Cursor{})
	if len(items) < 2 {
		t.Fatalf("session should have user input + assistant msg, got %d", len(items))
	}

	// Second run: history must be prepended to the model input.
	if _, err := RunSync(ctx, agent, "what did I say?", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
		t.Fatal(err)
	}
	// The second call's input should include the prior turn's items plus both
	// user messages.
	// 2 prior items (user "hello" + assistant) + new user message = 3.
	if len(model.lastReq.Input) < 3 {
		t.Errorf("second run input too short (history not prepended): %d", len(model.lastReq.Input))
	}
}
