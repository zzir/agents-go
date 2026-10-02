package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func statusOf(t *testing.T, runner *Runner, owner, id string) SessionState {
	t.Helper()
	states, err := runner.SessionStatuses(context.Background(), owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The single-session read must agree with the listing.
	one, err := runner.SessionStatuses(context.Background(), store.EveryOwner, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := one.Of(id).Status, states.Of(id).Status; got != want {
		t.Fatalf("status by id = %q, by owner = %q", got, want)
	}
	return states.Of(id)
}

func mkSession(t *testing.T, sessions *store.SessionStore, owner, name string, hidden bool) *store.Session {
	t.Helper()
	sess := &store.Session{ID: store.NewID(), OwnerID: owner, Name: name, Hidden: hidden}
	if err := sessions.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

// appendAs writes entries to a session the way a run with runID does.
func appendAs(t *testing.T, runner *Runner, sessionID, runID string, entries ...session.Entry) {
	t.Helper()
	es := store.NewEntryStoreFor(runner.db, mustRef(t, runner.db, sessionID))
	es.SetRunID(runID)
	if err := es.Append(context.Background(), entries...); err != nil {
		t.Fatal(err)
	}
}

func userEntry(t *testing.T, text string) session.Entry {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"role": "user", "content": text})
	e, err := session.NewItemEntry(userInputItems(t, string(raw))[0], agents.Source{Type: agents.SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The status a conversation carries is derived in one place, from the durable
// rows and the hub (invariant 3), highest priority first.
func TestSessionStatuses(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, _ := newTaskTestRunner(t)
	owner := store.LocalUserID

	t.Run("a new conversation is idle", func(t *testing.T) {
		sess := mkSession(t, sessions, owner, "idle", false)
		st := statusOf(t, runner, owner, sess.ID)
		if st.Status != protocol.SessionIdle || st.LiveRunID != "" || len(st.Pending) != 0 {
			t.Errorf("state = %+v, want idle with nothing live or pending", st)
		}
	})

	t.Run("a live run is running, and names the run", func(t *testing.T) {
		sess := mkSession(t, sessions, owner, "running", false)
		runID := store.NewID()
		seg, _, err := runner.hub.register(runID, sess.ID, owner, "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionRunning || st.LiveRunID != runID {
			t.Errorf("state = %+v, want running on %s", st, runID)
		}
		runner.hub.finish(runID, false)
		seg.finalize()
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionIdle {
			t.Errorf("after the run: %q, want idle", st.Status)
		}
	})

	t.Run("a paused run requires action and lists its call", func(t *testing.T) {
		sess := mkSession(t, sessions, owner, "paused", false)
		runID := store.NewID()
		appendAs(t, runner, sess.ID, runID, userEntry(t, "clean up"))
		pausedRun(t, runner, sess.ID, runID)
		st := statusOf(t, runner, owner, sess.ID)
		if st.Status != protocol.SessionRequiresAction || st.LiveRunID != "" {
			t.Fatalf("state = %+v, want requires_action with no live run", st)
		}
		if len(st.Pending) != 1 || st.Pending[0].ToolName != "exec_command" || st.Pending[0].SessionID != sess.ID || st.Pending[0].TaskID != "" {
			t.Fatalf("pending = %+v, want the one exec_command call of this conversation", st.Pending)
		}
		// The default TTL is on, so the call says when it expires.
		if st.Pending[0].ExpiresAt == nil || !st.Pending[0].ExpiresAt.After(st.Pending[0].CreatedAt) {
			t.Errorf("expires_at = %v, want a time after created_at", st.Pending[0].ExpiresAt)
		}
		w := st.Wire(sess.ID)
		if w.PendingCount != 1 || w.OldestPendingAt == nil {
			t.Errorf("wire = %+v, want one pending call with its age", w)
		}
	})

	t.Run("a pause whose run was branched away does not count", func(t *testing.T) {
		sess := mkSession(t, sessions, owner, "branched", false)
		before, runID := store.NewID(), store.NewID()
		appendAs(t, runner, sess.ID, before, userEntry(t, "first"))
		ref := mustRef(t, runner.db, sess.ID)
		es := store.NewSharedEntryStore(runner.db)
		anchor, err := es.Leaf(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		appendAs(t, runner, sess.ID, runID, userEntry(t, "second"))
		pausedRun(t, runner, sess.ID, runID)
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionRequiresAction {
			t.Fatalf("on path: %q, want requires_action", st.Status)
		}
		paused, err := es.Leaf(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := es.Branch(ctx, ref, anchor); err != nil {
			t.Fatal(err)
		}
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionIdle || len(st.Pending) != 0 {
			t.Errorf("branched away: %+v, want idle with nothing pending", st)
		}
		// Switching back re-admits the pause.
		if err := es.Branch(ctx, ref, paused); err != nil {
			t.Fatal(err)
		}
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionRequiresAction {
			t.Errorf("switched back: %q, want requires_action", st.Status)
		}
	})

	t.Run("a task moves its parent: working runs, input_required waits", func(t *testing.T) {
		parent := mkSession(t, sessions, owner, "parent", false)
		child := mkSession(t, sessions, owner, "child", true)
		runID := store.NewID()
		task := &store.Task{
			ID: store.NewID(), RunID: runID, ParentSessionID: parent.ID, ChildSessionID: child.ID,
			Label: "audit", Status: protocol.TaskWorking,
		}
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		if st := statusOf(t, runner, owner, parent.ID); st.Status != protocol.SessionRunning || st.LiveRunID != "" {
			t.Fatalf("working task: %+v, want running with no run of the conversation's own", st)
		}
		calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-task", ToolName: "exec_command", Arguments: `{}`}})
		won, err := tasks.Pause(ctx, task.ID, runID, nil, &store.PendingApproval{
			RunID: runID, SessionID: child.ID, State: "{}", ToolCalls: calls,
		})
		if err != nil || !won {
			t.Fatalf("pause: won=%v err=%v", won, err)
		}
		st := statusOf(t, runner, owner, parent.ID)
		if st.Status != protocol.SessionRequiresAction {
			t.Fatalf("paused task: %q, want requires_action", st.Status)
		}
		if len(st.Pending) != 1 || st.Pending[0].TaskID != task.ID || st.Pending[0].SessionID != parent.ID || st.Pending[0].TaskLabel != "audit" {
			t.Errorf("pending = %+v, want the task's call, opened through its parent", st.Pending)
		}
		// The hidden transcript is never a conversation of its own.
		if st := statusOf(t, runner, owner, child.ID); st.Status != protocol.SessionIdle {
			t.Errorf("hidden child: %q, want idle", st.Status)
		}
	})

	t.Run("a run that ended in error fails the conversation until the next run writes", func(t *testing.T) {
		sess := mkSession(t, sessions, owner, "failed", false)
		runID := store.NewID()
		appendAs(t, runner, sess.ID, runID, userEntry(t, "go"),
			session.NewAnnotationEntry(agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"}, agents.Source{Type: agents.SourceErrorHandler}))
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionFailed {
			t.Fatalf("after the error: %q, want failed", st.Status)
		}
		// An entry no run wrote (a task's display update) changes nothing.
		appendAs(t, runner, sess.ID, "", session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayMessage, Text: "note"}, agents.Source{Type: agents.SourceHost}))
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionFailed {
			t.Errorf("after a host note: %q, want failed still", st.Status)
		}
		// A cancellation is not a failure, and the next run's first write clears it.
		appendAs(t, runner, sess.ID, store.NewID(), userEntry(t, "again"))
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionIdle {
			t.Errorf("after the next run wrote: %q, want idle", st.Status)
		}
		appendAs(t, runner, sess.ID, store.NewID(),
			session.NewAnnotationEntry(agents.ItemDisplay{Kind: agents.DisplayCancelled}, agents.Source{Type: agents.SourceErrorHandler}))
		if st := statusOf(t, runner, owner, sess.ID); st.Status != protocol.SessionIdle {
			t.Errorf("after a cancellation: %q, want idle", st.Status)
		}
	})

	t.Run("another owner's conversations are not read", func(t *testing.T) {
		other := store.NewID()
		sess := mkSession(t, sessions, other, "theirs", false)
		runID := store.NewID()
		appendAs(t, runner, sess.ID, runID, userEntry(t, "clean up"))
		pausedRun(t, runner, sess.ID, runID)
		mine, err := runner.SessionStatuses(ctx, owner, nil)
		if err != nil {
			t.Fatal(err)
		}
		if st := mine.Of(sess.ID); st.Status != protocol.SessionIdle {
			t.Errorf("read through another owner: %q, want idle", st.Status)
		}
		for _, call := range mine.Pending() {
			if call.SessionID == sess.ID {
				t.Errorf("the inbox lists another owner's call: %+v", call)
			}
		}
		theirs, err := runner.SessionStatuses(ctx, other, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := theirs.Pending(); len(got) != 1 || got[0].SessionID != sess.ID {
			t.Errorf("their inbox = %+v, want their one call", got)
		}
	})
}

// statusRecorder collects the session.status broadcasts of one conversation.
type statusRecorder struct {
	mu   sync.Mutex
	seen []protocol.SessionStatus
}

func (r *statusRecorder) record(_ context.Context, env *protocol.Envelope, _, _ string) {
	if env.Type != protocol.EventSessionStatus {
		return
	}
	var st protocol.SessionStatus
	if json.Unmarshal(env.Payload, &st) != nil {
		return
	}
	r.mu.Lock()
	r.seen = append(r.seen, st)
	r.mu.Unlock()
}

func (r *statusRecorder) statuses(sessionID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, st := range r.seen {
		if st.SessionID == sessionID {
			out = append(out, st.Status)
		}
	}
	return out
}

// A pause and the decision on it each announce the conversation's status to
// every connection of its owner — no tab derives it from what it has loaded.
func TestSessionStatusIsBroadcast(t *testing.T) {
	srv := execSeriesModel(t, "make build")
	runner, sess, ac := trustFixture(t, srv.URL)
	rec := &statusRecorder{}
	runner.OnBroadcast = rec.record

	done := make(chan *RunOutcome, 1)
	runID, err := runner.StartRun(sess.ID, ac.ID, "", TextInput("build it"), nil, func(o *RunOutcome) { done <- o })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if !out.Interrupted {
			t.Fatalf("outcome = %+v, want a pause", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never paused")
	}
	awaitStatuses(t, rec, sess.ID, protocol.SessionRunning, protocol.SessionRequiresAction)

	call := pausedCommand(t, runner, sess.ID, runID)
	if out := decide(t, runner, runID, call, ApprovalOnce); out.Interrupted || out.ErrCode != "" {
		t.Fatalf("resumed outcome = %+v, want a clean finish", out)
	}
	awaitStatuses(t, rec, sess.ID, protocol.SessionRunning, protocol.SessionRequiresAction, protocol.SessionRunning, protocol.SessionIdle)
}

// awaitStatuses waits for the conversation's broadcasts to read want in order
// (the end-of-segment broadcast trails onDone).
func awaitStatuses(t *testing.T, rec *statusRecorder, sessionID string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := rec.statuses(sessionID)
		if len(got) >= len(want) {
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("statuses = %v, want %v", got, want)
				}
			}
			if len(got) > len(want) {
				t.Fatalf("statuses = %v, want exactly %v", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
