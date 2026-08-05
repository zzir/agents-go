package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// RunStatus is the lifecycle state of a run tracked by the hub.
type RunStatus string

const (
	// RunRunning means the run is executing.
	RunRunning RunStatus = "running"
	// RunInterrupted means the run paused awaiting tool approval (HITL).
	RunInterrupted RunStatus = "interrupted"
	// RunCompleted means the run finished and produced output.
	RunCompleted RunStatus = "completed"
	// RunErrored means the run ended with an error.
	RunErrored RunStatus = "error"
	// RunCancelled means the run was cancelled.
	RunCancelled RunStatus = "cancelled"
)

const (
	// EventBufferCap bounds the per-run replay ring buffer. A reconnecting
	// client can only be replayed the most recent EventBufferCap events;
	// consumers sizing their own delivery buffers (e.g. the SSE handler)
	// should match it so a full-buffer replay is lossless.
	EventBufferCap = 512
	// runRetention is how long a finished run stays queryable / replayable
	// after it ends, so late reconnects and status polls still work.
	runRetention = 15 * time.Minute
)

// terminalStatusForEvent maps a run's terminal event type to the RunStatus it
// drives, reporting false for any non-terminal event. It is the single
// authority for the run lifecycle: publish advances a run's status through it,
// and the IsTerminal/IsFinal predicates below derive from it, so the state
// machine is defined in exactly one place.
func terminalStatusForEvent(typ string) (RunStatus, bool) {
	switch typ {
	case protocol.EventRunOutput:
		return RunCompleted, true
	case protocol.EventRunError:
		return RunErrored, true
	case protocol.EventRunCancelled:
		return RunCancelled, true
	case protocol.EventRunInterrupted:
		return RunInterrupted, true
	}
	return "", false
}

// IsTerminalRunEvent reports whether typ ends a run's event stream for now:
// the run finished (output/error/cancelled) or paused for approval
// (interrupted — the approval decision resumes the SAME run id, continuing
// its sequence, so subscribers can reattach with their existing cursor).
func IsTerminalRunEvent(typ string) bool {
	_, ok := terminalStatusForEvent(typ)
	return ok
}

// IsFinalRunEvent reports whether typ ends a run for good — as opposed to
// run.interrupted, which only PAUSES it (same-id resume continues the stream).
// A stream that closes on a replayed run.interrupted would cut off before the
// resumed run.output; only a final event should terminate a live stream.
func IsFinalRunEvent(typ string) bool {
	st, ok := terminalStatusForEvent(typ)
	return ok && st != RunInterrupted
}

// SeqEnvelope is a hub event tagged with its per-run sequence number, used as
// the resume cursor (WS from_seq / SSE Last-Event-ID).
type SeqEnvelope struct {
	Seq int                `json:"seq"`
	Env *protocol.Envelope `json:"env"`
}

// TaskMeta links a background task run to the parent chat session/run that
// spawned it. Nil for ordinary runs.
type TaskMeta struct {
	TaskID          string `json:"task_id,omitempty"`
	ParentSessionID string `json:"parent_session_id"`
	ParentRunID     string `json:"parent_run_id,omitempty"`
	ToolCallID      string `json:"tool_call_id,omitempty"`
	Label           string `json:"label,omitempty"`
}

// RunInfo is a snapshot of a run's identity and state for status queries.
type RunInfo struct {
	RunID         string    `json:"run_id"`
	SessionID     string    `json:"session_id"`
	AgentConfigID string    `json:"agent_config_id,omitempty"`
	SandboxID     string    `json:"sandbox_id,omitempty"`
	Status        RunStatus `json:"status"`
	LastSeq       int       `json:"last_seq"`
	// GracefulStop records that StopAfterTurn was requested: a clean finish
	// after it is a cancellation (postRun writes the task row accordingly),
	// not a completion. Internal to the stop flow.
	GracefulStop bool `json:"-"`
	// Task is set for background task runs (SessionID is then the task's own
	// hidden session; Task carries the parent linkage).
	Task *TaskMeta `json:"task,omitempty"`
}

// TaskStatusFor maps a run status onto the MCP Tasks (SEP-1686) five-state
// task vocabulary — the single point where the two state models meet.
func TaskStatusFor(s RunStatus) string {
	switch s {
	case RunInterrupted:
		return protocol.TaskInputRequired
	case RunCompleted:
		return protocol.TaskCompleted
	case RunErrored:
		return protocol.TaskFailed
	case RunCancelled:
		return protocol.TaskCancelled
	default:
		return protocol.TaskWorking
	}
}

// newRunFanout builds a run's event broadcaster. Replay is sized to
// EventBufferCap so a reconnecting client can be caught up; each subscriber
// gets the same allowance, so a connection has to fall a full buffer behind
// before it loses anything.
func newRunFanout() *agents.Fanout[*protocol.Envelope] {
	return agents.NewFanout[*protocol.Envelope](agents.FanoutOptions{
		Replay:     EventBufferCap,
		Subscriber: EventBufferCap,
	})
}

// SeqSink receives a hub event together with its sequence number. It is the
// seq-aware form of EventSink used by SSE (which needs the seq for the
// Last-Event-ID id line).
type SeqSink func(SeqEnvelope)

// runSegment is one execution of a run: the goroutine that started it (a fresh
// StartRun or an approval resume) owns the segment and is the ONLY closer of
// its done gate and the ONLY caller of its cancel. Because the closer captures
// its OWN done/cancel by value, a resume that swaps a fresh segment onto the
// record can never make two goroutines race one channel (the double-close)
// or leak a finished segment's cancel context: each goroutine cancels and
// closes exactly the segment it started.
type runSegment struct {
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once
}

// finalize releases the segment: it cancels the segment's context (so the
// context tree rooted at the hub root sheds this child instead of leaking it)
// and closes the segment's done gate. Idempotent and safe to call from exactly
// one goroutine — the one that started the segment.
func (s *runSegment) finalize() {
	s.once.Do(func() {
		s.cancel()
		close(s.done)
	})
}

// runRecord is the hub's per-run state: its cancel hook, the replay buffer,
// and the live subscribers fanned out to.
type runRecord struct {
	info   RunInfo
	cancel context.CancelFunc
	// done mirrors the CURRENT segment's done gate (see runSegment): it closes
	// when that segment's goroutine has fully finished (postRun included), so
	// the session-delete path can wait on the live segment. A resume replaces it
	// with the new segment's gate; the old goroutine still closes its own
	// captured gate, never this one.
	done chan struct{}
	// ctrl is the live run's RunControl, set by the run goroutine once the run
	// exists. It is how an uplink message reaches a run that is already going:
	// a graceful stop, or input injected into one of the three queues.
	// Distinct from cancel, which is a hard context abort.
	ctrl agents.RunControl

	// fanout owns everything about delivery: sequence assignment, the replay
	// ring, per-subscriber buffering, and the slow-subscriber policy. The hub
	// used to hand-roll all of it; agents.Fanout is that machinery, single
	// sourced, so a subscriber that falls behind is TOLD (a *agents.GapError on
	// its own stream) instead of silently losing events.
	fanout *agents.Fanout[*protocol.Envelope]

	mu      sync.Mutex
	endedAt time.Time
}

// RunHub owns the lifecycle of active and recently-finished runs: it enforces
// one live run per session, buffers events for replay, and fans them out to
// subscribers independent of any single connection. A run's context descends
// from the hub's root context, so a dropped client never cancels a run.
type RunHub struct {
	rootCtx context.Context

	// maxTasks caps live background tasks per parent session (--max-tasks;
	// set once at construction time, read under mu with everything else).
	maxTasks int

	mu        sync.Mutex
	runs      map[string]*runRecord
	bySession map[string]string // sessionID -> live run id (only while running)
	// draining latches when Shutdown begins: no new run may register or
	// resume from then on, so the drain's snapshot is the complete set.
	draining bool
	// deleting marks sessions whose delete cascade is in progress. register and
	// resume refuse them so a task's postRun drain (or any late resume) cannot
	// start a fresh run on a session that is about to be removed. The set
	// grows by one entry per deleted session over the process lifetime — a
	// deleted session id is never reused, so the entries stay valid and the
	// growth is bounded by the number of distinct deletes.
	deleting map[string]bool
}

// NewRunHub returns a hub scoped to rootCtx and starts its GC loop.
func NewRunHub(rootCtx context.Context) *RunHub {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	h := &RunHub{
		rootCtx:   rootCtx,
		maxTasks:  tasks.DefaultMaxConcurrentPerParent,
		runs:      make(map[string]*runRecord),
		bySession: make(map[string]string),
		deleting:  make(map[string]bool),
	}
	go h.gcLoop()
	return h
}

// Shutdown cancels every live run and waits — bounded by ctx — for each
// one's goroutine to finish, final persistence included. SIGTERM used to
// close only the HTTP listener: nothing cancelled the runs, savePartialTurn
// never happened, and db.Close() executed under still-writing goroutines, so
// an interrupted turn simply vanished from its session.
func (h *RunHub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	// Latch FIRST: a cancelled run's postRun drains its task notifications,
	// which starts a wake-up run. Registered after the snapshot, it would be
	// neither cancelled nor waited on — and its notification debt is marked
	// delivered as it starts, so the process exiting under it loses the
	// notification for good. From here on, register and resume refuse.
	h.draining = true
	recs := make([]*runRecord, 0, len(h.runs))
	for _, rec := range h.runs {
		recs = append(recs, rec)
	}
	h.mu.Unlock()

	// Status decides only whether to CANCEL; the wait covers every segment
	// gate regardless. A run's status flips to terminal when its terminal
	// EVENT publishes — before the goroutine has persisted approvals,
	// finalized its task row and closed the gate — so waiting only on
	// "running" records would let shutdown close the database under exactly
	// the goroutines still writing their endings. Waiting on an
	// already-closed gate costs nothing. Reads are under rec.mu, the lock the
	// event path and resume mutate these fields under.
	var gates []chan struct{}
	for _, rec := range recs {
		rec.mu.Lock()
		if rec.info.Status == RunRunning && rec.cancel != nil {
			rec.cancel()
		}
		if rec.done != nil {
			gates = append(gates, rec.done)
		}
		rec.mu.Unlock()
	}
	for _, gate := range gates {
		select {
		case <-gate:
		case <-ctx.Done():
			return
		}
	}
}

// markSessionDeleting records that sessionID's delete cascade has begun, so no
// new run (fresh or resumed) is registered against it while it is torn down.
func (h *RunHub) markSessionDeleting(sessionID string) {
	h.mu.Lock()
	h.deleting[sessionID] = true
	h.mu.Unlock()
}

// unmarkSessionDeleting clears the mark after a delete cascade FAILED: the
// store rolled back, the session still exists, and leaving the mark would
// refuse every future run on it until the process restarts. (A successful
// delete never clears it — the id is never reused.)
func (h *RunHub) unmarkSessionDeleting(sessionID string) {
	h.mu.Lock()
	delete(h.deleting, sessionID)
	h.mu.Unlock()
}

// SessionDeleting reports whether a session's delete cascade is in progress.
func (h *RunHub) SessionDeleting(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deleting[sessionID]
}

// ErrSessionDeleting is returned by register/resume when the session is being
// torn down by a delete cascade — a new run must not be started on it.
type ErrSessionDeleting struct{ SessionID string }

func (e ErrSessionDeleting) Error() string {
	return "session is being deleted: " + e.SessionID
}

// ErrShuttingDown is returned by register/resume once Shutdown has begun. A
// run started after the drain took its snapshot would never be waited on, and
// the process would exit under it — exactly the vanished turn the drain
// exists to prevent. The wake-up path is the live case: a drained run's
// postRun drains its task notifications, which launches a NEW run.
type ErrShuttingDown struct{}

func (ErrShuttingDown) Error() string { return "server is shutting down" }

// ErrSessionBusy is returned by register when the session already has a live run.
type ErrSessionBusy struct{ RunID string }

func (e ErrSessionBusy) Error() string { return "session already has an active run: " + e.RunID }

// ErrTaskLimit is returned by register when the parent session is already at
// its live-task cap. Enforced inside the hub lock so concurrent spawns in one
// turn cannot collectively overshoot (check-then-act would).
type ErrTaskLimit struct{ Limit int }

func (e ErrTaskLimit) Error() string {
	return fmt.Sprintf("session already has %d live tasks; wait for one to finish or stop one", e.Limit)
}

// register creates a run record for a fresh run on sessionID and returns the
// segment the caller's goroutine owns plus a context descending from the hub
// root (not any connection). The goroutine MUST call seg.finalize() exactly
// once when it ends. It fails with ErrSessionBusy if the session already has a
// live run, or ErrSessionDeleting if the session is being torn down.
func (h *RunHub) register(runID, sessionID, agentConfigID, sandboxID string, task *TaskMeta) (*runSegment, context.Context, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return nil, nil, ErrShuttingDown{}
	}
	if h.deleting[sessionID] {
		return nil, nil, ErrSessionDeleting{SessionID: sessionID}
	}
	if existing, ok := h.bySession[sessionID]; ok {
		return nil, nil, ErrSessionBusy{RunID: existing}
	}
	if task != nil && h.liveTaskCountLocked(task.ParentSessionID) >= h.maxTasks {
		return nil, nil, ErrTaskLimit{Limit: h.maxTasks}
	}
	ctx, cancel := context.WithCancel(h.rootCtx)
	seg := &runSegment{done: make(chan struct{}), cancel: cancel}
	rec := &runRecord{
		info:   RunInfo{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, Status: RunRunning, Task: task},
		cancel: seg.cancel,
		done:   seg.done,
		fanout: newRunFanout(),
	}
	h.runs[runID] = rec
	h.bySession[sessionID] = runID
	return seg, ctx, nil
}

// LiveTaskCount reports how many live (running or input-required) task runs
// belong to the given parent session.
func (h *RunHub) LiveTaskCount(parentSessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.liveTaskCountLocked(parentSessionID)
}

func (h *RunHub) liveTaskCountLocked(parentSessionID string) int {
	n := 0
	for _, rec := range h.runs {
		rec.mu.Lock()
		if rec.info.Task != nil && rec.info.Task.ParentSessionID == parentSessionID &&
			(rec.info.Status == RunRunning || rec.info.Status == RunInterrupted) {
			n++
		}
		rec.mu.Unlock()
	}
	return n
}

// resume reopens an interrupted run so its continuation streams under the
// same run id: one logical run keeps one id across interrupt/resume, so
// events, traces, and messages never need to be re-keyed. The record keeps
// its sequence counter, replay buffer, and subscribers — attached clients
// simply keep receiving events, and SSE Last-Event-ID cursors stay valid.
// If the record is gone (server restart, retention GC), a fresh record is
// created under the same id with the sequence restarting at zero. The caller's
// goroutine owns the returned segment and MUST call seg.finalize() when it ends.
// It fails with ErrSessionBusy if the session already has a live run,
// ErrSessionDeleting if the session is being torn down, or ErrRunNotResumable
// if the record is not paused (a concurrent stop finished it).
func (h *RunHub) resume(runID, sessionID, agentConfigID, sandboxID string, task *TaskMeta) (*runSegment, context.Context, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return nil, nil, ErrShuttingDown{}
	}
	if h.deleting[sessionID] {
		return nil, nil, ErrSessionDeleting{SessionID: sessionID}
	}
	if existing, ok := h.bySession[sessionID]; ok {
		return nil, nil, ErrSessionBusy{RunID: existing}
	}
	ctx, cancel := context.WithCancel(h.rootCtx)
	seg := &runSegment{done: make(chan struct{}), cancel: cancel}
	rec := h.runs[runID]
	if rec == nil {
		rec = &runRecord{
			info:   RunInfo{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, Status: RunRunning, Task: task},
			cancel: seg.cancel,
			done:   seg.done,
			fanout: newRunFanout(),
		}
		h.runs[runID] = rec
		h.bySession[sessionID] = runID
		return seg, ctx, nil
	}
	rec.mu.Lock()
	// Only a paused run resumes. A record finished by a concurrent stop (or a
	// completed/errored one) stays dead — reviving it would let an approve
	// race resurrect a task the user just cancelled.
	if rec.info.Status != RunInterrupted {
		st := rec.info.Status
		rec.mu.Unlock()
		cancel()
		return nil, nil, ErrRunNotResumable{RunID: runID, Status: st}
	}
	rec.cancel = seg.cancel
	rec.info.Status = RunRunning
	rec.info.GracefulStop = false
	// Drop the previous segment's control: it belongs to the old run and would
	// steer or stop the wrong one. The new segment installs its own via
	// setControl once its run exists.
	rec.ctrl = nil
	// A fresh segment gets a fresh done gate; the previous segment's goroutine
	// still owns and closes its own (see runSegment), so this swap is safe.
	rec.done = seg.done
	if task != nil {
		rec.info.Task = task
	}
	rec.endedAt = time.Time{}
	rec.mu.Unlock()
	h.bySession[sessionID] = runID
	return seg, ctx, nil
}

// ErrRunNotResumable is returned by resume when a run's segment is not paused
// (Interrupted) — e.g. a concurrent stop finalized it. Handlers map it to 409:
// the run reached a terminal state and cannot be continued.
type ErrRunNotResumable struct {
	RunID  string
	Status RunStatus
}

func (e ErrRunNotResumable) Error() string {
	return fmt.Sprintf("run %s is %s and cannot be resumed", e.RunID, e.Status)
}

// publish assigns the next sequence number to env, appends it to the run's
// replay buffer, advances the terminal status for terminal event types, and
// fans the event out to all current subscribers.
//
// Three contracts hold it together:
//
//   - Ordering belongs to agents.Fanout.Publish, which holds one lock from
//     sequence assignment through delivery. Concurrent publishes on one run are
//     therefore serialized, and no subscriber can see a higher seq before a
//     lower one. The record lock below is taken only to mutate the record,
//     never during fan-out, so subscribe/info stay responsive while events flow.
//   - A sink runs on its own subscriber goroutine (see SubscribeSeq), never on
//     the publisher's. Delivery into the per-subscriber buffer is a
//     non-blocking send, so a sink that blocks deliberately — the SSE one does,
//     to back-pressure into that buffer instead of dropping a second time —
//     costs only its own buffer, and overflowing it surfaces as a run.gap
//     rather than as silent loss.
//   - The status advance runs AFTER Publish returns, under the record lock
//     alone: there is no lock shared with other publishers. LastSeq tolerates
//     that because it re-reads the fanout's counter instead of this event's own
//     number, so it cannot run backwards. Status rests on something weaker — a
//     convention that at most one publisher emits a TERMINAL event for a run at
//     a time. The run's own goroutine holds that role while its segment is live
//     (the title goroutine publishes alongside it, but only
//     session.title_updated); publishTaskCancelled takes it for a run whose
//     segment has ended. The handover between the two is check-then-act, not
//     locked: taskStopper.Stop reads the status through Info and publishes
//     after, so a stop that saw RunInterrupted just before an approval resumed
//     the run can still land a terminal event beside a live segment. What
//     catches that is the post-resume re-check in ResolveApproval, which
//     cancels the run it just started once the task row reads terminal — a
//     compensation, not this lock. A new publisher able to emit a terminal
//     event beside a live segment needs an atomic status transition here
//     instead of a third compensation.
func (h *RunHub) publish(runID string, env *protocol.Envelope) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return
	}

	rec.fanout.Publish(env)

	rec.mu.Lock()
	if st, ok := terminalStatusForEvent(env.Type); ok {
		rec.info.Status = st
	}
	rec.info.LastSeq = rec.fanout.LastSeq()
	rec.mu.Unlock()
}

// Subscribe attaches a plain sink (seq discarded) to the run's live event
// stream. See SubscribeSeq.
func (h *RunHub) Subscribe(runID string, fromSeq int, sink EventSink) (func(), bool) {
	return h.SubscribeSeq(runID, fromSeq, func(item SeqEnvelope) { sink(item.Env) })
}

// SubscribeSeq attaches sink to the run's live event stream after replaying
// any buffered events with seq > fromSeq (pass 0 to replay everything
// retained). It returns the detach function — Go's cancel-closure idiom, the
// same shape the fanout itself hands back — and whether the run exists.
// Detaching is idempotent; the fanout's Close ends every remaining subscriber
// when the run is garbage-collected.
//
// The sink runs on its own goroutine, fed by the subscriber's buffer, so a slow
// sink no longer runs on the publishing goroutine and cannot affect its peers.
// If it falls far enough behind that its buffer overflows, it is delivered a
// gap event naming the range it missed — the client resubscribes from LastSeq
// rather than rendering a timeline that is quietly incomplete.
func (h *RunHub) SubscribeSeq(runID string, fromSeq int, sink SeqSink) (func(), bool) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return nil, false
	}

	stream, cancel := rec.fanout.Subscribe(fromSeq)

	go func() {
		for item, err := range stream {
			var gap *agents.GapError
			if errors.As(err, &gap) {
				env, mkErr := protocol.NewEnvelope(protocol.EventRunGap, protocol.RunGap{
					RunID:    runID,
					Dropped:  gap.Dropped,
					LastGood: gap.LastGood,
					Next:     gap.Next,
				})
				if mkErr == nil {
					sink(SeqEnvelope{Seq: gap.LastGood, Env: env})
				}
				// A gap that runs to the end of the stream has no item after
				// it: the value alongside it is the zero one, a nil envelope
				// here. Forwarding it would hand every sink a nil to
				// dereference — and the sinks run on this goroutine, so the
				// panic takes the process down rather than one request.
				if gap.AtEnd() {
					continue
				}
			}
			sink(SeqEnvelope{Seq: item.Seq, Env: item.Value})
		}
	}()

	return cancel, true
}

// finish marks a run terminal: interrupted when it paused for approval,
// otherwise keeping the status publish already derived (falling back to
// completed). It frees the session's live-run slot and starts the retention
// clock.
func (h *RunHub) finish(runID string, interrupted bool) {
	h.mu.Lock()
	rec := h.runs[runID]
	if rec != nil {
		delete(h.bySession, rec.info.SessionID)
	}
	h.mu.Unlock()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	if interrupted {
		rec.info.Status = RunInterrupted
	} else if rec.info.Status == RunRunning {
		rec.info.Status = RunCompleted
	}
	rec.endedAt = time.Now()
	rec.mu.Unlock()
}

// waitDone blocks until the run's current segment has fully finished or the
// deadline passes. Unknown runs (GC'd, restarted away) return immediately.
func (h *RunHub) waitDone(runID string, deadline time.Time) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	done := rec.done
	rec.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
}

// Cancel cancels the run's context, which unwinds the run goroutine. It
// reports whether a live run with that id existed.
func (h *RunHub) Cancel(runID string) bool {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return false
	}
	// resume swaps rec.cancel to the new segment's cancel under rec.mu, so read
	// it under the same lock — an unsynchronized read here races that write and
	// could cancel the wrong segment. Invoke it outside the lock.
	rec.mu.Lock()
	cancel := rec.cancel
	rec.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// setControl records a live run's control handle (the run goroutine calls this
// once its run exists).
func (h *RunHub) setControl(runID string, ctrl agents.RunControl) {
	h.mu.Lock()
	if rec := h.runs[runID]; rec != nil {
		rec.ctrl = ctrl
	}
	h.mu.Unlock()
}

// control returns a live run's control handle, or nil.
func (h *RunHub) control(runID string) agents.RunControl {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec := h.runs[runID]; rec != nil {
		return rec.ctrl
	}
	return nil
}

// Inject delivers input to a live run through one of RunControl's three
// queues. Reports whether a live run was there to receive it.
//
// The queue is chosen by the caller, not inferred: steer changes course now,
// next-turn rides along with a turn the run was taking anyway, and follow-up
// starts the next exchange. They are different intentions and the client says
// which one it means.
func (h *RunHub) Inject(runID, queue string, input any) (bool, error) {
	ctrl := h.control(runID)
	if ctrl == nil {
		return false, nil
	}
	switch queue {
	case protocol.InjectQueueSteer:
		return true, ctrl.Steer(input)
	case protocol.InjectQueueNextTurn:
		return true, ctrl.NextTurn(input)
	case protocol.InjectQueueFollowUp:
		return true, ctrl.FollowUp(input)
	default:
		return false, fmt.Errorf("unknown injection queue %q", queue)
	}
}

// StopAfterTurn requests a graceful stop of a live run: the current turn
// finishes and the run ends cleanly before the next one. Reports whether a live
// run with a stop hook existed. Falls back to nothing (returns false) for a run
// that has not yet installed its hook.
func (h *RunHub) StopAfterTurn(runID string) bool {
	h.mu.Lock()
	var ctrl agents.RunControl
	if rec := h.runs[runID]; rec != nil {
		rec.mu.Lock()
		if rec.ctrl != nil {
			// Mark before signalling: the run goroutine's postRun must never
			// observe a clean finish without the graceful-stop marker.
			rec.info.GracefulStop = true
			ctrl = rec.ctrl
		}
		rec.mu.Unlock()
	}
	h.mu.Unlock()
	if ctrl == nil {
		return false
	}
	ctrl.StopAfterTurn()
	return true
}

// Info returns a snapshot of the run's state, or false if unknown/expired.
func (h *RunHub) Info(runID string) (RunInfo, bool) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return RunInfo{}, false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.info, true
}

// ActiveRunForSession returns the id of the session's live run, if any.
func (h *RunHub) ActiveRunForSession(sessionID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id, ok := h.bySession[sessionID]
	return id, ok
}

// LiveRunIDs returns the ids of every currently executing run (one per busy
// session). Used to attach a freshly connected client to all in-flight streams;
// interrupted runs are excluded — they re-enter this set when resumed, and the
// resume attach hook covers late joiners.
func (h *RunHub) LiveRunIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.bySession))
	for _, id := range h.bySession {
		ids = append(ids, id)
	}
	return ids
}

// gcLoop drops finished runs once they age past runRetention.
func (h *RunHub) gcLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.rootCtx.Done():
			// Shutdown: end every broadcaster so subscriber goroutines exit.
			h.mu.Lock()
			for _, rec := range h.runs {
				rec.fanout.Close()
			}
			h.mu.Unlock()
			return
		case <-ticker.C:
			now := time.Now()
			h.mu.Lock()
			for id, rec := range h.runs {
				rec.mu.Lock()
				expired := !rec.endedAt.IsZero() && now.Sub(rec.endedAt) > runRetention
				rec.mu.Unlock()
				if expired {
					delete(h.runs, id)
					// Close the broadcaster too, or the goroutine feeding each
					// still-attached sink blocks forever on a record nothing
					// can reach any more.
					rec.fanout.Close()
				}
			}
			h.mu.Unlock()
		}
	}
}
