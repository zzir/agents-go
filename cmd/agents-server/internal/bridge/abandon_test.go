package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// pausedRun seeds what a run paused for approval leaves behind: the approval
// row with its pending call, and the hub's interrupted record with a subscriber.
func pausedRun(t *testing.T, runner *Runner, sessionID, runID string) <-chan *protocol.Envelope {
	t.Helper()
	ctx := context.Background()
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-" + runID, ToolName: "exec_command", Arguments: `{"cmd":"rm -rf build"}`}})
	if err := runner.Deps.PendingApprovals.Save(ctx, &store.PendingApproval{
		RunID: runID, SessionID: sessionID, State: "{}", ToolCalls: calls, UserInput: "clean up",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.hub.register(runID, sessionID, "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	got := make(chan *protocol.Envelope, 8)
	if _, ok := runner.hub.Subscribe(runID, 0, func(env *protocol.Envelope) { got <- env }); !ok {
		t.Fatal("subscribe")
	}
	runner.hub.finish(runID, true)
	return got
}

func cancelledReason(t *testing.T, got <-chan *protocol.Envelope) string {
	t.Helper()
	select {
	case env := <-got:
		var p protocol.RunCancelled
		if env.Type != protocol.EventRunCancelled || json.Unmarshal(env.Payload, &p) != nil {
			t.Fatalf("event = %s %s, want run.cancelled", env.Type, env.Payload)
		}
		return p.Reason
	case <-time.After(2 * time.Second):
		t.Fatal("no run.cancelled reached the subscriber")
		return ""
	}
}

// A newer message on a session whose run is paused for approval abandons the
// pause (invariant 19): the approval row goes, the pending call persists as
// not run with the reason, and the hub ends the run with run.cancelled
// {reason: superseded}. An explicit cancel of the paused run does the same
// with reason stopped, and adds the cancelled marker a stop always leaves.
func TestAbandonPausedApproval(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	approvals := store.NewPendingApprovalStore(db)
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs: store.NewAgentConfigStore(db), Providers: store.NewProviderStore(db), Sessions: sessions,
		Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db), PendingApprovals: approvals,
	})
	newSession := func() string {
		sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
		if err := sessions.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		return sess.ID
	}
	entriesOf := func(sessionID string) []session.Entry {
		t.Helper()
		entries, err := mustStore(t, db, sessionID).Entries(ctx, session.Cursor{})
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	kinds := func(entries []session.Entry) (notRun, cancelled int, reason string, prompt bool) {
		for _, e := range entries {
			if e.Kind == session.EntryKindItem {
				prompt = true
			}
			if e.Display == nil {
				continue
			}
			switch e.Display.Kind {
			case session.DisplayToolCall:
				if r, ok := e.Display.Extra["not_run"].(string); ok {
					notRun++
					reason = r
				}
			case session.DisplayCancelled:
				cancelled++
			}
		}
		return
	}

	// Superseded by a newer message.
	sid := newSession()
	got := pausedRun(t, runner, sid, "paused-1")
	runner.abandonPaused(ctx, sid, protocol.RunCancelSuperseded)
	if _, err := approvals.Get(ctx, "paused-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the approval row must be gone, got %v", err)
	}
	if r := cancelledReason(t, got); r != protocol.RunCancelSuperseded {
		t.Fatalf("reason = %q, want superseded", r)
	}
	if info, _ := runner.hub.Info("paused-1"); info.Status != RunCancelled {
		t.Fatalf("hub status = %q, want cancelled", info.Status)
	}
	if notRun, cancelled, reason, prompt := kinds(entriesOf(sid)); notRun != 1 || reason != protocol.RunCancelSuperseded || cancelled != 0 || !prompt {
		t.Fatalf("persisted turn: not_run=%d reason=%q cancelled=%d prompt=%v, want one superseded call, no marker, the prompt", notRun, reason, cancelled, prompt)
	}
	if _, _, err := runner.ResolveApproval(ctx, "call-paused-1", true, ApprovalOnce, "", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a decision after the abandon = %v, want not found", err)
	}

	// Stopped by an explicit cancel of the paused run.
	sid = newSession()
	got = pausedRun(t, runner, sid, "paused-2")
	runner.CancelRun("paused-2")
	if r := cancelledReason(t, got); r != protocol.RunCancelStopped {
		t.Fatalf("reason = %q, want stopped", r)
	}
	if notRun, cancelled, reason, _ := kinds(entriesOf(sid)); notRun != 1 || reason != protocol.RunCancelStopped || cancelled != 1 {
		t.Fatalf("persisted turn: not_run=%d reason=%q cancelled=%d, want one stopped call and the marker", notRun, reason, cancelled)
	}

	// A graceful stop has no turn to finish on a paused run: it abandons the same way.
	sid = newSession()
	got = pausedRun(t, runner, sid, "paused-4")
	runner.StopRunAfterTurn("paused-4")
	if r := cancelledReason(t, got); r != protocol.RunCancelStopped {
		t.Fatalf("graceful stop on a pause: reason = %q, want stopped", r)
	}
	if _, err := approvals.Get(ctx, "paused-4"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the approval row must be gone after a graceful stop, got %v", err)
	}

	// A decision that claimed the row first wins: nothing to abandon.
	sid = newSession()
	got = pausedRun(t, runner, sid, "paused-3")
	if err := approvals.Delete(ctx, "paused-3"); err != nil {
		t.Fatal(err)
	}
	runner.abandonPaused(ctx, sid, protocol.RunCancelSuperseded)
	if info, _ := runner.hub.Info("paused-3"); info.Status != RunInterrupted {
		t.Fatalf("hub status = %q, want the pause untouched", info.Status)
	}
	select {
	case env := <-got:
		t.Fatalf("nothing should be published, got %s", env.Type)
	case <-time.After(100 * time.Millisecond):
	}
	if n, _, _, _ := kinds(entriesOf(sid)); n != 0 {
		t.Fatal("nothing should be persisted")
	}
}
