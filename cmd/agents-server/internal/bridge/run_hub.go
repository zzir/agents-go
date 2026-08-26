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
	// EventBufferCap bounds the per-run replay ring buffer. Consumers sizing
	// their own delivery buffers (e.g. the SSE handler) should match it so a
	// full-buffer replay is lossless.
	EventBufferCap = 512
	// runRetention is how long a finished run stays queryable / replayable
	// after it ends.
	runRetention = 15 * time.Minute
)

// terminalStatusForEvent maps a run's terminal event type to the RunStatus it
// drives, reporting false for any non-terminal event. Single authority for the
// run lifecycle: publish and the IsTerminal/IsFinal predicates derive from it.
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
// Only a final event should terminate a live stream.
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
	TaskID string `json:"task_id,omitempty"`
	// Kind is the task's kind: "" a sub-agent task, "workflow" an execution.
	Kind            string `json:"kind,omitempty"`
	ParentSessionID string `json:"parent_session_id"`
	ParentRunID     string `json:"parent_run_id,omitempty"`
	ToolCallID      string `json:"tool_call_id,omitempty"`
	Label           string `json:"label,omitempty"`
	// Attempt is which run of the task this is: 1 for the original, more after a
	// retry. It rides on run.started so a client can tell a NEW attempt from a
	// replay of the old one.
	Attempt int `json:"attempt,omitempty"`
	// MaxAttempts is the ceiling Attempt is measured against.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// RunInfo is a snapshot of a run's identity and state for status queries.
type RunInfo struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	// OwnerID is the session's owner: who may watch this run's stream (WS
	// attach, SSE) and act on it. Empty attaches nobody — fail closed.
	OwnerID       string    `json:"-"`
	AgentConfigID string    `json:"agent_config_id,omitempty"`
	SandboxID     string    `json:"sandbox_id,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	Status        RunStatus `json:"status"`
	LastSeq       int       `json:"last_seq"`
	// GracefulStop records that StopAfterTurn was requested: a clean finish
	// after it is a cancellation, not a completion (postRun writes the task row
	// accordingly).
	GracefulStop bool `json:"-"`
	// Task is set for background task runs (SessionID is then the task's own
	// hidden session; Task carries the parent linkage).
	Task *TaskMeta `json:"task,omitempty"`
}

// isTerminalRunStatus reports whether a run has ended: nothing can be
// cancelled, and publishing a cancellation would rewrite what clients saw.
func isTerminalRunStatus(s RunStatus) bool {
	switch s {
	case RunCompleted, RunErrored, RunCancelled:
		return true
	}
	return false
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

// newRunFanout builds a run's event broadcaster, sizing replay and each
// subscriber's buffer to EventBufferCap.
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

// runSegment is one execution of a run. The goroutine that started it (a fresh
// StartRun or an approval resume) is the sole caller of its cancel and sole
// closer of its done gate, capturing both by value — so a resume swapping in a
// fresh segment cannot race the double-close or leak the old cancel context.
type runSegment struct {
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once
}

// finalize cancels the segment's context and closes its done gate. Idempotent;
// called once, by the goroutine that started the segment.
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
	// done mirrors the CURRENT segment's done gate (see runSegment), so the
	// session-delete path can wait on the live segment. A resume swaps in the
	// new segment's gate.
	done chan struct{}
	// ctrl is the live run's RunControl, set once the run exists: how an uplink
	// message (graceful stop, or injected input) reaches a run already going.
	// Distinct from cancel, a hard context abort.
	ctrl agents.RunControl

	// fanout owns delivery: sequence assignment, the replay ring, per-subscriber
	// buffering, and the slow-subscriber policy — a subscriber that falls behind
	// is TOLD (a *agents.GapError on its own stream) instead of losing events.
	fanout *agents.Fanout[*protocol.Envelope]

	mu      sync.Mutex
	endedAt time.Time
	// started is the run's latest run.started, pinned OUTSIDE the ring with its
	// seq: the one event a subscriber from 0 must get even after the ring has
	// moved past it (workbench invariant 14).
	started    *protocol.Envelope
	startedSeq int
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
	// start a fresh run on a session about to be removed.
	deleting map[string]bool
}

// MaxTasks reports the per-parent live-task cap in force — the flag when one
// was given, the built-in default otherwise. Resolved here rather than at each
// reader, so nothing has to re-derive what "0 means default" came to.
func (h *RunHub) MaxTasks() int { return h.maxTasks }

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
// one's goroutine to finish, final persistence included.
func (h *RunHub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	// Latch FIRST, before snapshotting: from here register and resume refuse, so
	// a wake-up run spawned by a cancelled run's postRun drain cannot slip in
	// after the snapshot and exit un-waited-on.
	h.draining = true
	recs := make([]*runRecord, 0, len(h.runs))
	for _, rec := range h.runs {
		recs = append(recs, rec)
	}
	h.mu.Unlock()

	// Status decides only whether to CANCEL; the wait covers every segment gate
	// regardless, because status flips to terminal when the terminal event
	// publishes — before the goroutine finishes persisting. Reads under rec.mu.
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
wait:
	for _, gate := range gates {
		select {
		case <-gate:
		case <-ctx.Done():
			break wait
		}
	}
	// Every broadcaster ends now — those of interrupted runs included, which
	// the drain neither cancels nor waits for — so a subscriber (an SSE
	// stream) returns instead of holding the HTTP shutdown to its deadline.
	h.mu.Lock()
	for _, rec := range h.runs {
		rec.fanout.Close()
	}
	h.mu.Unlock()
}

// markSessionDeleting records that sessionID's delete cascade has begun, so no
// new run (fresh or resumed) is registered against it while it is torn down.
func (h *RunHub) markSessionDeleting(sessionID string) {
	h.mu.Lock()
	h.deleting[sessionID] = true
	h.mu.Unlock()
}

// unmarkSessionDeleting clears the mark after a delete cascade FAILED and the
// session still exists. A successful delete never clears it — the id is never
// reused.
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

// deletingLocked refuses a run whose session — or, for a task run, whose PARENT
// session — is being torn down. Callers hold h.mu. The parent check catches a
// FAILED task's retry (invisible to the teardown's task-stop) from writing into
// rows the cascade is removing.
func (h *RunHub) deletingLocked(sessionID string, task *TaskMeta) error {
	if h.deleting[sessionID] {
		return ErrSessionDeleting{SessionID: sessionID}
	}
	if task != nil && task.ParentSessionID != "" && h.deleting[task.ParentSessionID] {
		return ErrSessionDeleting{SessionID: task.ParentSessionID}
	}
	return nil
}

// ErrSessionDeleting is returned by register/resume when the session is being
// torn down by a delete cascade — a new run must not be started on it.
type ErrSessionDeleting struct{ SessionID string }

func (e ErrSessionDeleting) Error() string {
	return "session is being deleted: " + e.SessionID
}

// ErrShuttingDown is returned by register/resume once Shutdown has begun: a run
// started after the drain's snapshot would exit un-waited-on. The live case is a
// drained run's postRun launching a wake-up run.
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
func (h *RunHub) register(runID, sessionID, ownerID, agentConfigID, sandboxID, projectID string, task *TaskMeta) (*runSegment, context.Context, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return nil, nil, ErrShuttingDown{}
	}
	if err := h.deletingLocked(sessionID, task); err != nil {
		return nil, nil, err
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
		info:   RunInfo{RunID: runID, SessionID: sessionID, OwnerID: ownerID, AgentConfigID: agentConfigID, SandboxID: sandboxID, ProjectID: projectID, Status: RunRunning, Task: task},
		cancel: seg.cancel,
		done:   seg.done,
		fanout: newRunFanout(),
	}
	h.runs[runID] = rec
	h.bySession[sessionID] = runID
	return seg, ctx, nil
}

// unregister withdraws a run that never launched: register succeeded but a
// pre-launch step failed, so nothing was published, no subscriber attached
// (OnRunAttach has not run), and no goroutine owns the segment. It releases
// the session slot and the segment's context so the failed start leaves no
// trace — the run id was never observable.
func (h *RunHub) unregister(runID string, seg *runSegment) {
	h.mu.Lock()
	if rec := h.runs[runID]; rec != nil {
		delete(h.runs, runID)
		if h.bySession[rec.info.SessionID] == runID {
			delete(h.bySession, rec.info.SessionID)
		}
		rec.fanout.Close()
	}
	h.mu.Unlock()
	seg.finalize()
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

// resume reopens an interrupted run so its continuation streams under the same
// run id, keeping the record's sequence counter, replay buffer, and subscribers
// so attached clients and SSE Last-Event-ID cursors stay valid. If the record
// is gone (server restart, retention GC), a fresh one is created under the same
// id with the sequence restarting at zero — reopened reports which of the two
// happened, so a withdrawal (abortResume) knows what to put back. The caller's
// goroutine owns the returned segment and MUST call seg.finalize() when it
// ends. Fails with ErrSessionBusy, ErrSessionDeleting, or ErrRunNotResumable
// (record not paused).
func (h *RunHub) resume(runID, sessionID, ownerID, agentConfigID, sandboxID, projectID string, task *TaskMeta) (seg *runSegment, ctx context.Context, reopened bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return nil, nil, false, ErrShuttingDown{}
	}
	if err := h.deletingLocked(sessionID, task); err != nil {
		return nil, nil, false, err
	}
	if existing, ok := h.bySession[sessionID]; ok {
		return nil, nil, false, ErrSessionBusy{RunID: existing}
	}
	ctx, cancel := context.WithCancel(h.rootCtx)
	seg = &runSegment{done: make(chan struct{}), cancel: cancel}
	rec := h.runs[runID]
	if rec == nil {
		rec = &runRecord{
			info:   RunInfo{RunID: runID, SessionID: sessionID, OwnerID: ownerID, AgentConfigID: agentConfigID, SandboxID: sandboxID, ProjectID: projectID, Status: RunRunning, Task: task},
			cancel: seg.cancel,
			done:   seg.done,
			fanout: newRunFanout(),
		}
		h.runs[runID] = rec
		h.bySession[sessionID] = runID
		return seg, ctx, false, nil
	}
	rec.mu.Lock()
	// Only a paused run resumes: reviving a finished record would let an approve
	// race resurrect a task the user just cancelled.
	if rec.info.Status != RunInterrupted {
		st := rec.info.Status
		rec.mu.Unlock()
		cancel()
		return nil, nil, false, ErrRunNotResumable{RunID: runID, Status: st}
	}
	rec.cancel = seg.cancel
	rec.info.Status = RunRunning
	rec.info.GracefulStop = false
	// Drop the old segment's control (it would steer the wrong run); the new
	// segment installs its own via setControl.
	rec.ctrl = nil
	// Fresh segment, fresh done gate; the old goroutine still closes its own.
	rec.done = seg.done
	if task != nil {
		rec.info.Task = task
	}
	rec.endedAt = time.Time{}
	rec.mu.Unlock()
	h.bySession[sessionID] = runID
	return seg, ctx, true, nil
}

// abortResume withdraws a resume whose pre-launch verify refused: the segment
// never ran and published nothing, so the record goes back to what resume
// found. A REOPENED record returns to interrupted with its history, fanout and
// subscribers intact — the pause they observe is still the truth — while a
// record resume CREATED (a post-restart resume) is withdrawn entirely, like
// any failed fresh start.
func (h *RunHub) abortResume(runID string, seg *runSegment, reopened bool) {
	if !reopened {
		h.unregister(runID, seg)
		return
	}
	h.mu.Lock()
	rec := h.runs[runID]
	if rec != nil {
		if h.bySession[rec.info.SessionID] == runID {
			delete(h.bySession, rec.info.SessionID)
		}
	}
	h.mu.Unlock()
	if rec != nil {
		rec.mu.Lock()
		rec.info.Status = RunInterrupted
		rec.ctrl = nil
		rec.endedAt = time.Now()
		rec.mu.Unlock()
	}
	// Closes the fresh segment's done gate (rec.done points at it) and releases
	// its context; the record itself stays.
	seg.finalize()
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
// fans the event out to all current subscribers. It reports whether the hub
// holds the run — false means nobody heard, and the caller decides.
//
// Locking: agents.Fanout.Publish serializes sequence assignment through
// delivery, so subscribers never see events out of order; the record lock is
// taken only for the status/LastSeq mutation after, never during fan-out. The
// status advance assumes at most one publisher emits a terminal event per run at
// a time — held by the run's own goroutine while its segment is live, and by
// publishTaskCancelled once the segment has ended. A stop racing an approval
// resume is caught by the post-resume re-check in ResolveApproval, not here.
func (h *RunHub) publish(runID string, env *protocol.Envelope) bool {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return false
	}

	rec.fanout.Publish(env)

	rec.mu.Lock()
	if st, ok := terminalStatusForEvent(env.Type); ok {
		rec.info.Status = st
	}
	rec.info.LastSeq = rec.fanout.LastSeq()
	if env.Type == protocol.EventRunStarted {
		rec.started, rec.startedSeq = env, rec.info.LastSeq
	}
	rec.mu.Unlock()
	return true
}

// Subscribe attaches a plain sink (seq discarded) to the run's live event
// stream. See SubscribeSeq.
func (h *RunHub) Subscribe(runID string, fromSeq int, sink EventSink) (func(), bool) {
	return h.SubscribeSeq(runID, fromSeq, func(item SeqEnvelope) { sink(item.Env) })
}

// SubscribeSeq attaches sink to the run's live event stream after replaying any
// buffered events with seq > fromSeq (pass 0 to replay everything retained). It
// returns the detach function (idempotent) and whether the run exists.
//
// The sink runs on its own goroutine, fed by the subscriber's buffer, so a slow
// sink cannot affect its peers. If its buffer overflows it gets a gap event
// naming the range it missed, and resubscribes from LastSeq rather than
// rendering a quietly incomplete timeline.
//
// A cursor before the run's latest run.started gets that event first whenever
// the ring no longer holds it — the run's identity is never lost to the ring
// (workbench invariant 14). Whether the ring holds it is read off the stream's
// opening item, which the fanout fixed at subscribe time, so no publish can
// slip between the check and the replay.
func (h *RunHub) SubscribeSeq(runID string, fromSeq int, sink SeqSink) (func(), bool) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return nil, false
	}

	var pinned *SeqEnvelope
	rec.mu.Lock()
	if rec.started != nil && fromSeq < rec.startedSeq {
		pinned = &SeqEnvelope{Seq: rec.startedSeq, Env: rec.started}
	}
	rec.mu.Unlock()

	stream, cancel := rec.fanout.Subscribe(fromSeq)

	go func() {
		for item, err := range stream {
			if pinned != nil {
				if item.Value == nil || item.Seq > pinned.Seq {
					sink(*pinned)
				}
				pinned = nil
			}
			if gap, ok := errors.AsType[*agents.GapError](err); ok {
				env, mkErr := protocol.NewEnvelope(protocol.EventRunGap, protocol.RunGap{
					RunID:    runID,
					Dropped:  gap.Dropped,
					LastGood: gap.LastGood,
					Next:     gap.Next,
				})
				if mkErr == nil {
					sink(SeqEnvelope{Seq: gap.LastGood, Env: env})
				}
			}
			// A gap can arrive on an item that carries nothing — at the end of
			// the stream, or the timeline reset a cursor ahead of the head gets
			// (from_seq is the CLIENT's number). The gap itself was delivered
			// above; forwarding the empty item would hand the sink a nil
			// envelope to dereference, and the sink runs on a goroutine of its
			// own, past any recovery.
			if item.Value == nil {
				continue
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
	// resume swaps rec.cancel under rec.mu, so read it under the same lock or
	// risk cancelling the wrong segment. Invoke outside the lock.
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

// Inject delivers input to a live run through one of RunControl's three queues
// (steer, next-turn, follow-up), chosen by the caller. Reports whether a live
// run was there to receive it.
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
// session), to attach a freshly connected client to all in-flight streams.
// Interrupted runs are excluded — they re-enter this set when resumed.
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
