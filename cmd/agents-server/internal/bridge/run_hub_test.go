package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

func env(typ string) *protocol.Envelope { return &protocol.Envelope{Type: typ} }

func TestRunHubBufferAndReplay(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if info, _ := h.Info("run1"); info.Status != RunRunning {
		t.Fatalf("new run status = %q", info.Status)
	}

	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.step"))

	// A subscriber attaching mid-run replays the buffered events, then sees
	// live ones.
	sink := newAsyncSink()
	cancelSub, ok := h.Subscribe("run1", 0, sink.plain)
	if !ok {
		t.Fatal("subscribe failed")
	}
	if !sink.wait(t, 2) {
		t.Fatalf("replay delivered %d of 2", sink.count())
	}
	if got := sink.gotTypes(); got[0] != "run.started" || got[1] != "run.step" {
		t.Fatalf("replay wrong: %v", got)
	}
	h.publish("run1", env("run.output"))
	if !sink.wait(t, 3) {
		t.Fatalf("live delivery landed %d of 3", sink.count())
	}
	if got := sink.gotTypes(); got[2] != "run.output" {
		t.Fatalf("live delivery wrong: %v", got)
	}

	// Terminal event advanced status; finish keeps it.
	h.finish("run1", false)
	if info, _ := h.Info("run1"); info.Status != RunCompleted {
		t.Fatalf("status after output = %q, want completed", info.Status)
	}

	// from_seq replays only events after the cursor.
	cursor := newAsyncSink()
	if _, ok := h.SubscribeSeq("run1", 2, cursor.seq); !ok {
		t.Fatal("cursor subscribe failed")
	}
	if !cursor.wait(t, 1) {
		t.Fatal("from_seq replay delivered nothing")
	}
	if after := cursor.gotSeqs(); len(after) != 1 || after[0] != 3 {
		t.Fatalf("from_seq replay wrong: %v", after)
	}

	cancelSub()
	h.publish("run1", env("run.step"))
	// Give a delivery that must NOT happen a chance to happen.
	time.Sleep(50 * time.Millisecond)
	if n := sink.count(); n != 3 {
		t.Fatalf("unsubscribed sink still received events: %v", sink.gotTypes())
	}

	// A cursor AHEAD of the head — the client's number, after a restart
	// recreated the stream, or simply wrong — is a timeline reset: the sink
	// gets a run.gap that says where to resume (Next), never the empty item
	// the reset rides on (this sink dereferences Env.Type, as the SSE handler
	// does — a nil there would panic the hub's goroutine, past any recovery).
	ahead := newAsyncSink()
	if _, ok := h.SubscribeSeq("run1", 999, ahead.seq); !ok {
		t.Fatal("ahead subscribe failed")
	}
	if !ahead.wait(t, 1) {
		t.Fatal("a cursor past the head must be answered with a gap")
	}
	if got := ahead.gotTypes(); got[0] != protocol.EventRunGap {
		t.Fatalf("ahead cursor got %v, want run.gap first", got)
	}
	var gapPayload protocol.RunGap
	_ = json.Unmarshal(ahead.envs[0].Env.Payload, &gapPayload)
	if gapPayload.LastGood != 0 || gapPayload.Next == 0 {
		t.Fatalf("gap = %+v, want a reset (LastGood 0, Next set)", gapPayload)
	}
	// And the stream is live after it: the next publish arrives.
	h.publish("run1", env("run.step"))
	if !ahead.wait(t, 2) {
		t.Fatalf("after the reset gap the live stream must continue, got %v", ahead.gotTypes())
	}
}

func TestRunHubSessionBusy(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, _, err := h.register("run2", "sess1", "", "", "", "", nil)
	if err == nil {
		t.Fatal("expected ErrSessionBusy for second live run on same session")
	}
	var busy ErrSessionBusy
	if !errors.As(err, &busy) || busy.RunID != "run1" {
		t.Fatalf("wrong error: %v", err)
	}

	// After the first run finishes, the session frees up.
	h.finish("run1", false)
	if _, _, err := h.register("run2", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register after finish: %v", err)
	}
}

func TestRunHubCancelAndInterrupt(t *testing.T) {
	h := NewRunHub(context.Background())
	_, ctx, err := h.register("run1", "sess1", "", "", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if !h.Cancel("run1") {
		t.Fatal("cancel should report the run existed")
	}
	if ctx.Err() == nil {
		t.Fatal("cancel should have cancelled the run context")
	}
	if h.Cancel("nope") {
		t.Fatal("cancel of unknown run should report false")
	}

	// Interrupt path sets the interrupted status.
	h.finish("run1", true)
	if info, _ := h.Info("run1"); info.Status != RunInterrupted {
		t.Fatalf("status after interrupt = %q, want interrupted", info.Status)
	}
}

func TestRunHubInterruptedEventIsTerminal(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	h.publish("run1", env("run.interrupted"))
	if info, _ := h.Info("run1"); info.Status != RunInterrupted {
		t.Fatalf("run.interrupted should set status, got %q", info.Status)
	}

	for _, typ := range []string{"run.output", "run.error", "run.cancelled", "run.interrupted"} {
		if !IsTerminalRunEvent(typ) {
			t.Errorf("%s should be terminal", typ)
		}
	}
	if IsTerminalRunEvent("run.step") {
		t.Error("run.step must not be terminal")
	}
}

// TestRunHubResumeSameID locks the same-id resume contract: an interrupted
// run reopens under its own id, keeping the sequence counter, replay buffer,
// and attached subscribers — one logical run, one event stream.
func TestRunHubResumeSameID(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "ac", "sb", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.tool_call"))
	h.publish("run1", env("run.interrupted"))

	// A client attached during the first segment...
	sink := newAsyncSink()
	if _, ok := h.Subscribe("run1", 3, sink.plain); !ok {
		t.Fatal("subscribe failed")
	}

	h.finish("run1", true)
	if info, _ := h.Info("run1"); info.Status != RunInterrupted {
		t.Fatalf("status after pause = %q, want interrupted", info.Status)
	}
	if _, busy := h.ActiveRunForSession("sess1"); busy {
		t.Fatal("interrupted run must free the session slot")
	}

	// Resume reopens the same record: same id, seq continues, sub still fed.
	_, ctx, _, err := h.resume("run1", "sess1", "", "ac", "sb", "", nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if ctx == nil {
		t.Fatal("resume returned nil context")
	}
	if info, _ := h.Info("run1"); info.Status != RunRunning {
		t.Fatalf("status after resume = %q, want running", info.Status)
	}
	if id, ok := h.ActiveRunForSession("sess1"); !ok || id != "run1" {
		t.Fatalf("session slot = %q,%v — want run1 reclaimed", id, ok)
	}
	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.output"))
	if !sink.wait(t, 2) {
		t.Fatalf("existing subscriber got %d of 2 resumed events", sink.count())
	}
	if got := sink.gotTypes(); got[0] != "run.started" || got[1] != "run.output" {
		t.Fatalf("existing subscriber missed resumed events: %v", got)
	}
	if info, _ := h.Info("run1"); info.LastSeq != 5 {
		t.Fatalf("seq restarted (LastSeq=%d), must continue from the first segment", info.LastSeq)
	}
	h.finish("run1", false)
	if info, _ := h.Info("run1"); info.Status != RunCompleted {
		t.Fatalf("final status = %q", info.Status)
	}
}

// A resume must respect the one-live-run-per-session invariant.
func TestRunHubResumeBusy(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("paused", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register paused: %v", err)
	}
	h.finish("paused", true)
	if _, _, err := h.register("blocker", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register blocker: %v", err)
	}
	if _, _, _, err := h.resume("paused", "sess1", "", "", "", "", nil); !errors.As(err, &ErrSessionBusy{}) {
		t.Fatalf("resume while busy: err = %v, want ErrSessionBusy", err)
	}
	h.finish("blocker", false)
	if _, _, _, err := h.resume("paused", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("resume after free: %v", err)
	}
}

// The SSE close decision must key off IsFinalRunEvent, not IsTerminalRunEvent:
// a same-id resume leaves a historical run.interrupted in the buffer, and a
// late subscriber replaying from 0 must flow past it to the real run.output —
// treating the old interrupt as final would cut the stream short.
func TestFinalVsTerminalRunEvent(t *testing.T) {
	// run.interrupted is "terminal" (ends a segment) but NOT "final" (the run
	// resumes under the same id).
	if !IsTerminalRunEvent("run.interrupted") {
		t.Error("run.interrupted should be terminal (segment end)")
	}
	if IsFinalRunEvent("run.interrupted") {
		t.Error("run.interrupted must NOT be final — same-id resume continues the stream")
	}
	for _, typ := range []string{"run.output", "run.error", "run.cancelled"} {
		if !IsFinalRunEvent(typ) {
			t.Errorf("%s should be final", typ)
		}
	}
	if IsFinalRunEvent("run.step") {
		t.Error("run.step is not final")
	}

	// A resumed+completed run's buffer: interrupted (seq 2) then output (seq 4).
	// Replaying from 0, only the output is a final event — the SSE would close
	// on it, delivering everything including the resume's output.
	h := NewRunHub(context.Background())
	if _, _, err := h.register("r", "s", "", "", "", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.publish("r", env("run.started"))
	h.publish("r", env("run.interrupted"))
	h.finish("r", true)
	_, _, _, _ = h.resume("r", "s", "", "", "", "", nil)
	h.publish("r", env("run.started"))
	h.publish("r", env("run.output"))

	sink := newAsyncSink()
	if _, ok := h.SubscribeSeq("r", 0, sink.seq); !ok {
		t.Fatal("subscribe failed")
	}
	if !sink.wait(t, 4) {
		t.Fatalf("only %d of 4 replayed events delivered", sink.count())
	}
	var finals []int
	sink.mu.Lock()
	for _, item := range sink.envs {
		if IsFinalRunEvent(item.Env.Type) {
			finals = append(finals, item.Seq)
		}
	}
	sink.mu.Unlock()
	if len(finals) != 1 || finals[0] != 4 {
		t.Fatalf("final events on replay = %v, want [4] (the run.output, not the old interrupt)", finals)
	}
}

// After a restart (or retention GC) the hub has no record: resume registers a
// fresh one under the same id so the continuation still streams.
func TestRunHubResumeAfterRestart(t *testing.T) {
	h := NewRunHub(context.Background())
	_, ctx, _, err := h.resume("ghost-run", "sess1", "", "ac", "", "", nil)
	if err != nil {
		t.Fatalf("resume without record: %v", err)
	}
	if ctx == nil {
		t.Fatal("nil context")
	}
	info, ok := h.Info("ghost-run")
	if !ok || info.Status != RunRunning || info.SessionID != "sess1" {
		t.Fatalf("fresh record wrong: %+v ok=%v", info, ok)
	}
}

// StopAfterTurn invokes the run's graceful-stop hook once installed, and reports
// false for a run that has none yet (the caller then falls back to a hard cancel).
func TestRunHubStopAfterTurn(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	// No hook installed yet.
	if h.StopAfterTurn("run1") {
		t.Error("StopAfterTurn should report false before a hook is installed")
	}
	called := 0
	h.setControl("run1", &fakeControl{onStop: func() { called++ }})
	if !h.StopAfterTurn("run1") {
		t.Error("StopAfterTurn should report true once a hook is installed")
	}
	if called != 1 {
		t.Errorf("stop hook called %d times, want 1", called)
	}
	// Unknown run.
	if h.StopAfterTurn("nope") {
		t.Error("StopAfterTurn on an unknown run should report false")
	}
}

// TestStopAfterTurnSetsGracefulMarker locks the graceful-stop contract: the
// marker lands on the record BEFORE the stop hook fires, so a clean finish
// can never be observed without it — postRun turns that finish into a
// cancelled terminal state. resume clears it for the next segment.
func TestStopAfterTurnSetsGracefulMarker(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("r1", "s1", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	var markerAtHook bool
	h.setControl("r1", &fakeControl{onStop: func() {
		info, _ := h.Info("r1")
		markerAtHook = info.GracefulStop
	}})
	if !h.StopAfterTurn("r1") {
		t.Fatal("StopAfterTurn found no hook")
	}
	if !markerAtHook {
		t.Fatal("graceful marker not visible at hook time")
	}
	if info, _ := h.Info("r1"); !info.GracefulStop {
		t.Fatal("graceful marker not retained")
	}
}

// TestResumeRefusesFinishedRecord locks the second line of defence in the
// stop/approve race: a record a stop already finished cannot be revived by a
// racing resume.
func TestResumeRefusesFinishedRecord(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("r2", "s2", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.EventRunCancelled, protocol.RunCancelled{RunID: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	h.publish("r2", env)
	h.finish("r2", false)
	if _, _, _, err := h.resume("r2", "s2", "", "", "", "", nil); err == nil {
		t.Fatal("resume revived a cancelled record")
	}
	// An interrupted record resumes fine.
	if _, _, err := h.register("r3", "s3", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	h.finish("r3", true)
	if _, _, _, err := h.resume("r3", "s3", "", "", "", "", nil); err != nil {
		t.Fatalf("resume of interrupted record: %v", err)
	}
}

// asyncSink is a test sink that records deliveries and lets a test wait for
// them. Each subscriber's sink runs on its own goroutine, fed by its own
// buffer, so that a slow sink never runs on the publishing goroutine — which
// means a test must wait for delivery rather than reading a slice straight
// after Subscribe or publish returns.
type asyncSink struct {
	mu    sync.Mutex
	types []string
	seqs  []int
	envs  []SeqEnvelope
	bell  chan struct{}
}

func newAsyncSink() *asyncSink { return &asyncSink{bell: make(chan struct{}, 1024)} }

func (a *asyncSink) seq(item SeqEnvelope) {
	a.mu.Lock()
	a.types = append(a.types, item.Env.Type)
	a.seqs = append(a.seqs, item.Seq)
	a.envs = append(a.envs, item)
	a.mu.Unlock()
	select {
	case a.bell <- struct{}{}:
	default:
	}
}

func (a *asyncSink) plain(e *protocol.Envelope) { a.seq(SeqEnvelope{Env: e}) }

func (a *asyncSink) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.types)
}

// wait blocks until n deliveries have landed, or the deadline passes. It
// returns whether the count was reached, so a test can assert "exactly n" by
// waiting for n and then confirming no extras arrive.
func (a *asyncSink) wait(t *testing.T, n int) bool {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for a.count() < n {
		select {
		case <-a.bell:
		case <-deadline:
			return false
		}
	}
	return true
}

func (a *asyncSink) gotTypes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.types...)
}

func (a *asyncSink) gotSeqs() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int(nil), a.seqs...)
}

// fakeControl is a stand-in RunControl for hub tests: it records what was asked
// of it without needing a real run behind it.
type fakeControl struct {
	stops     int
	steer     []any
	nextTurn  []any
	followUp  []any
	onStop    func()
	failQueue bool
}

func (c *fakeControl) StopAfterTurn() {
	c.stops++
	if c.onStop != nil {
		c.onStop()
	}
}
func (c *fakeControl) Pending() agents.PendingInput { return agents.PendingInput{} }

func (c *fakeControl) Steer(in any) error { c.steer = append(c.steer, in); return c.queueErr() }
func (c *fakeControl) NextTurn(in any) error {
	c.nextTurn = append(c.nextTurn, in)
	return c.queueErr()
}
func (c *fakeControl) FollowUp(in any) error {
	c.followUp = append(c.followUp, in)
	return c.queueErr()
}

func (c *fakeControl) queueErr() error {
	if c.failQueue {
		return errors.New("queue rejected")
	}
	return nil
}

// The three queues are distinct semantics, so the hub routes by message type
// rather than collapsing them into one endpoint with a mode.
func TestInjectRoutesToTheRightQueue(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("r1", "s1", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	ctrl := &fakeControl{}
	h.setControl("r1", ctrl)

	for _, tc := range []struct {
		queue string
		got   *[]any
	}{
		{protocol.InjectQueueSteer, &ctrl.steer},
		{protocol.InjectQueueNextTurn, &ctrl.nextTurn},
		{protocol.InjectQueueFollowUp, &ctrl.followUp},
	} {
		delivered, err := h.Inject("r1", tc.queue, "hello")
		if err != nil || !delivered {
			t.Fatalf("%s: delivered=%v err=%v", tc.queue, delivered, err)
		}
		if len(*tc.got) != 1 {
			t.Errorf("%s went to the wrong queue", tc.queue)
		}
	}
}

// A run that has finished cannot receive input, and the caller is told so —
// the user typed something and it must not vanish.
func TestInjectReportsNoLiveRun(t *testing.T) {
	h := NewRunHub(context.Background())
	if delivered, err := h.Inject("nope", protocol.InjectQueueSteer, "hi"); delivered || err != nil {
		t.Errorf("delivered=%v err=%v, want (false, nil) for an unknown run", delivered, err)
	}
	if _, _, err := h.register("r1", "s1", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	// Registered but no control installed yet.
	if delivered, _ := h.Inject("r1", protocol.InjectQueueSteer, "hi"); delivered {
		t.Error("delivered to a run that has not started")
	}
}

func TestInjectRejectsAnUnknownQueue(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("r1", "s1", "", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	h.setControl("r1", &fakeControl{})
	if _, err := h.Inject("r1", "run.whatever", "hi"); err == nil {
		t.Error("an unknown queue was accepted")
	}
}
