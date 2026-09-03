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

// terminalStatusForEvent maps a terminal event type to the RunStatus it drives,
// false for a non-terminal one; publish and IsFinalRunEvent derive from it.
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

// IsFinalRunEvent reports whether typ ends a run for good — as opposed to
// run.interrupted, which only PAUSES it (the approval decision resumes the
// SAME run id, continuing its sequence). Only a final event should terminate
// a live stream.
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
	// Attempt is which run of the task this is (1 for the original); on
	// run.started it tells a NEW attempt from a replay of the old one.
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
	ProjectID     string    `json:"project_id,omitempty"`
	Status        RunStatus `json:"status"`
	LastSeq       int       `json:"last_seq"`
	// GracefulStop records a StopAfterTurn request: a clean finish after it is
	// a cancellation, not a completion (postRun writes the task row so).
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

// runSegment is one execution of a run. The goroutine that started it is the
// sole caller of cancel and sole closer of done, so a resume cannot race either.
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
	// done mirrors the CURRENT segment's gate (a resume swaps it), so the
	// session-delete path waits on the live segment.
	done chan struct{}
	// ctrl is the live run's RunControl (graceful stop, injected input); distinct
	// from cancel, the hard abort. Under mu, like info.
	ctrl agents.RunControl

	// fanout owns delivery: seq assignment, the replay ring, per-subscriber
	// buffers; a slow subscriber gets a *agents.GapError, never silent loss.
	fanout *agents.Fanout[*protocol.Envelope]

	mu      sync.Mutex
	endedAt time.Time
	// started is the latest run.started pinned OUTSIDE the ring with its seq,
	// for a subscriber whose cursor lies before it — invariant 14.
	started    *protocol.Envelope
	startedSeq int
}

// RunHub owns the lifecycle of active and recently-finished runs: it enforces
// one live run per session, buffers events for replay, and fans them out to
// subscribers independent of any single connection. A run's context descends
// from the hub's root context, so a dropped client never cancels a run.
type RunHub struct {
	rootCtx context.Context

	// maxTasks resolves the per-parent live-task cap at each register (a live
	// setting). Read OUTSIDE mu (it hits the DB), compared under mu.
	maxTasks func() int

	mu        sync.Mutex
	runs      map[string]*runRecord
	bySession map[string]string // sessionID -> live run id (only while running)
	// draining latches when Shutdown begins: no new run may register or
	// resume from then on, so the drain's snapshot is the complete set.
	draining bool
	// deleting marks sessions mid delete-cascade: register and resume refuse
	// them, so no late resume or postRun drain starts a run on one.
	deleting map[string]bool
}

// NewRunHub returns a hub scoped to rootCtx and starts its GC loop. The cap
// resolver defaults to the built-in; NewRunner overrides it with the
// settings-backed one.
func NewRunHub(rootCtx context.Context) *RunHub {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	h := &RunHub{
		rootCtx:   rootCtx,
		maxTasks:  func() int { return tasks.DefaultMaxConcurrentPerParent },
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
	// Latch FIRST, before snapshotting: a wake-up run spawned by a drained
	// run's postRun must not slip in after the snapshot, un-waited-on.
	h.draining = true
	recs := make([]*runRecord, 0, len(h.runs))
	for _, rec := range h.runs {
		recs = append(recs, rec)
	}
	h.mu.Unlock()

	// Status decides only whether to CANCEL; every gate is waited on, since
	// status flips terminal before the goroutine finishes persisting.
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
	// End every broadcaster, interrupted runs' included, so a subscriber (an
	// SSE stream) returns instead of holding the HTTP shutdown to its deadline.
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

// unmarkSessionDeleting clears the mark after a delete cascade FAILED; a
// successful delete never clears it — the id is never reused.
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

// deletingLocked refuses a run whose session — or, for a task run, PARENT
// session — is being torn down (a failed task's retry). Callers hold h.mu.
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

// register creates a fresh run on sessionID, returning the caller's segment (finalize
// it exactly once) and a hub-root context; ErrSessionBusy when a run is already live.
func (h *RunHub) register(runID, sessionID, ownerID, agentConfigID, projectID string, task *TaskMeta) (*runSegment, context.Context, error) {
	// Resolve the cap before the lock: the resolver reads the DB, and h.mu gates
	// every register/deregister.
	var limit int
	if task != nil {
		limit = h.maxTasks()
	}
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
	if task != nil && h.liveTaskCountLocked(task.ParentSessionID) >= limit {
		return nil, nil, ErrTaskLimit{Limit: limit}
	}
	ctx, cancel := context.WithCancel(h.rootCtx)
	seg := &runSegment{done: make(chan struct{}), cancel: cancel}
	rec := &runRecord{
		info:   RunInfo{RunID: runID, SessionID: sessionID, OwnerID: ownerID, AgentConfigID: agentConfigID, ProjectID: projectID, Status: RunRunning, Task: task},
		cancel: seg.cancel,
		done:   seg.done,
		fanout: newRunFanout(),
	}
	h.runs[runID] = rec
	h.bySession[sessionID] = runID
	return seg, ctx, nil
}

// unregister withdraws a run that never launched (a pre-launch step failed
// after register): frees the session slot and the segment; nothing was published.
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

// liveTaskCountLocked counts the live (running or input-required) task runs
// of the given parent session. Callers hold h.mu.
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

// resume reopens an interrupted run under the same id; a record lost to restart or GC
// is recreated with seq from zero (reopened says which). Finalize seg exactly once.
func (h *RunHub) resume(runID, sessionID, ownerID, agentConfigID, projectID string, task *TaskMeta) (seg *runSegment, ctx context.Context, reopened bool, err error) {
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
			info:   RunInfo{RunID: runID, SessionID: sessionID, OwnerID: ownerID, AgentConfigID: agentConfigID, ProjectID: projectID, Status: RunRunning, Task: task},
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

// abortResume withdraws a resume whose verify refused: a REOPENED record goes
// back to interrupted, subscribers intact; a record resume created is dropped.
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

// publish rings env, fans it out, then advances the status under rec.mu (never held
// during fan-out); false means the hub has no such run. See invariant 23.
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
	cancel, _, ok := h.SubscribeSeq(runID, fromSeq, func(item SeqEnvelope) { sink(item.Env) })
	return cancel, ok
}

// SubscribeSeq attaches sink to the run's live event stream after replaying the
// buffered events with seq > fromSeq (0 replays everything retained). It returns
// the detach function (idempotent), a channel closed once the stream has ended
// (every event already handed to the sink), and whether the run exists. The
// sink runs on its own goroutine; an overflow reaches it as a run.gap, and a
// cursor before the latest run.started gets that event first — invariant 14.
func (h *RunHub) SubscribeSeq(runID string, fromSeq int, sink SeqSink) (func(), <-chan struct{}, bool) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return nil, nil, false
	}

	var pinned *SeqEnvelope
	rec.mu.Lock()
	if rec.started != nil && fromSeq < rec.startedSeq {
		pinned = &SeqEnvelope{Seq: rec.startedSeq, Env: rec.started}
	}
	rec.mu.Unlock()

	stream, cancel := rec.fanout.Subscribe(fromSeq)
	done := make(chan struct{})

	go func() {
		defer close(done)
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
			// An item can carry only a gap (end of stream, or a cursor past the
			// head); the sink runs on its own goroutine, so never hand it nil.
			if item.Value == nil {
				continue
			}
			sink(SeqEnvelope{Seq: item.Seq, Env: item.Value})
		}
	}()

	return cancel, done, true
}

// finish marks the run terminal — interrupted when paused, else what publish
// derived (default completed) — frees the session slot and starts retention.
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
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	rec.ctrl = ctrl
	rec.mu.Unlock()
}

// control returns a live run's control handle, or nil.
func (h *RunHub) control(runID string) agents.RunControl {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.ctrl
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
					// Close the broadcaster too, or each attached sink's feeder
					// goroutine blocks forever on an unreachable record.
					rec.fanout.Close()
				}
			}
			h.mu.Unlock()
		}
	}
}
