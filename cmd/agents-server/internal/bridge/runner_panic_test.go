package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A panicking run segment must fail that run alone: the goroutine recovers,
// the session slot frees, and the process survives to serve everything else.
func TestLaunchSegmentRecoversPanic(t *testing.T) {
	hub := NewRunHub(context.Background())
	seg, _, err := hub.register("run-1", "sess-1", "", "agent", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r := &Runner{hub: hub}
	r.launchSegment(seg, "run-1", "sess-1", nil, func() *RunOutcome {
		panic("boom")
	})
	hub.waitDone("run-1", time.Now().Add(2*time.Second))
	if _, busy := hub.ActiveRunForSession("sess-1"); busy {
		t.Fatal("session slot still held after a panicking segment")
	}
	// The slot must be genuinely reusable, not just unlisted.
	seg2, _, err := hub.register("run-2", "sess-1", "", "agent", "", nil)
	if err != nil {
		t.Fatalf("register after panic: %v", err)
	}
	hub.unregister("run-2", seg2)
}

// A panic inside a run segment — the SDK recovers a tool's own panic into a
// tool error, so this one is the bridge's: the build hook explodes — fails
// THAT run with a run.error, records the failure on the turn, and frees the
// session; the process and every other run carry on.
func TestRunPanicFailsTheRunWithAnError(t *testing.T) {
	ctx := context.Background()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	_, srv := newRecordingModel(t, func(int, []byte) []any { return sayOutput("ok") })
	pid := testProvider(t, runner.db, "endpoint", "k", srv.URL)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "worker", Model: "gpt-test", ProviderID: pid}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	runner.Deps.SpawnTool = func(context.Context, string) *agents.Tool { panic("build exploded") }

	var outcome *RunOutcome
	done := make(chan struct{})
	runID, err := runner.StartRun(sess.ID, ac.ID, "", "go", nil, func(o *RunOutcome) { outcome = o; close(done) })
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	var errEvent atomic.Pointer[protocol.RunError]
	detach, ok := runner.Hub().Subscribe(runID, 0, func(env *protocol.Envelope) {
		if env.Type == protocol.EventRunError {
			var re protocol.RunError
			_ = json.Unmarshal(env.Payload, &re)
			errEvent.Store(&re)
		}
	})
	if !ok {
		t.Fatal("run not subscribable")
	}
	defer detach()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end")
	}
	if outcome == nil || outcome.ErrCode != protocol.CodeInternal || !strings.Contains(outcome.ErrMessage, "build exploded") {
		t.Fatalf("outcome = %+v, want an internal failure carrying the panic", outcome)
	}
	deadline := time.Now().Add(2 * time.Second)
	for errEvent.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if re := errEvent.Load(); re == nil || re.Code != protocol.CodeInternal {
		t.Fatalf("run.error = %+v, want internal", re)
	}
	if _, busy := runner.Hub().ActiveRunForSession(sess.ID); busy {
		t.Fatal("session slot still held after the panic")
	}
	// The failure is on the record: the turn carries the error annotation.
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewSharedEntryStore(runner.db).GetEntries(ctx, ref, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, v := range views {
		if v.Display != nil && strings.Contains(v.Display.Text, "build exploded") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the panic left no annotation on the turn; entries = %+v", views)
	}
	// And the session runs again.
	runner.Deps.SpawnTool = nil
	again := make(chan struct{})
	if _, err := runner.StartRun(sess.ID, ac.ID, "", "again", nil, func(*RunOutcome) { close(again) }); err != nil {
		t.Fatalf("the session must accept a run after the panic: %v", err)
	}
	select {
	case <-again:
	case <-time.After(10 * time.Second):
		t.Fatal("the second run did not end")
	}
}
