package bridge

import (
	"context"
	"sync"
	"time"

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

// RunInfo is a snapshot of a run's identity and state for status queries.
type RunInfo struct {
	RunID         string    `json:"run_id"`
	SessionID     string    `json:"session_id"`
	AgentConfigID string    `json:"agent_config_id,omitempty"`
	SandboxID     string    `json:"sandbox_id,omitempty"`
	Status        RunStatus `json:"status"`
	LastSeq       int       `json:"last_seq"`
}

// SeqSink receives a hub event together with its sequence number. It is the
// seq-aware form of EventSink used by SSE (which needs the seq for the
// Last-Event-ID id line).
type SeqSink func(SeqEnvelope)

// runRecord is the hub's per-run state: its cancel hook, the replay buffer,
// and the live subscribers fanned out to.
type runRecord struct {
	info   RunInfo
	cancel context.CancelFunc
	// stopAfterTurn, when set by the run goroutine, requests a graceful stop:
	// the in-flight turn finishes (tools + session save) and the run ends cleanly
	// before the next turn. Distinct from cancel (a hard context abort).
	stopAfterTurn func()

	mu      sync.Mutex
	seq     int
	buffer  []SeqEnvelope
	subs    map[int]SeqSink
	nextSub int
	endedAt time.Time
}

// RunHub owns the lifecycle of active and recently-finished runs: it enforces
// one live run per session, buffers events for replay, and fans them out to
// subscribers independent of any single connection. A run's context descends
// from the hub's root context, so a dropped client never cancels a run.
type RunHub struct {
	rootCtx context.Context

	mu        sync.Mutex
	runs      map[string]*runRecord
	bySession map[string]string // sessionID -> live run id (only while running)
}

// NewRunHub returns a hub scoped to rootCtx and starts its GC loop.
func NewRunHub(rootCtx context.Context) *RunHub {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	h := &RunHub{
		rootCtx:   rootCtx,
		runs:      make(map[string]*runRecord),
		bySession: make(map[string]string),
	}
	go h.gcLoop()
	return h
}

// ErrSessionBusy is returned by register when the session already has a live run.
type ErrSessionBusy struct{ RunID string }

func (e ErrSessionBusy) Error() string { return "session already has an active run: " + e.RunID }

// register creates a run record for a fresh run on sessionID and returns it
// with a context descending from the hub root (not any connection). It fails
// with ErrSessionBusy if the session already has a live run.
func (h *RunHub) register(runID, sessionID, agentConfigID, sandboxID string) (*runRecord, context.Context, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.bySession[sessionID]; ok {
		return nil, nil, ErrSessionBusy{RunID: existing}
	}
	ctx, cancel := context.WithCancel(h.rootCtx)
	rec := &runRecord{
		info:   RunInfo{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, Status: RunRunning},
		cancel: cancel,
		subs:   make(map[int]SeqSink),
	}
	h.runs[runID] = rec
	h.bySession[sessionID] = runID
	return rec, ctx, nil
}

// resume reopens an interrupted run so its continuation streams under the
// same run id: one logical run keeps one id across interrupt/resume, so
// events, traces, and messages never need to be re-keyed. The record keeps
// its sequence counter, replay buffer, and subscribers — attached clients
// simply keep receiving events, and SSE Last-Event-ID cursors stay valid.
// If the record is gone (server restart, retention GC), a fresh record is
// created under the same id with the sequence restarting at zero.
// It fails with ErrSessionBusy if the session already has a live run.
func (h *RunHub) resume(runID, sessionID, agentConfigID, sandboxID string) (context.Context, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.bySession[sessionID]; ok {
		return nil, ErrSessionBusy{RunID: existing}
	}
	ctx, cancel := context.WithCancel(h.rootCtx)
	rec := h.runs[runID]
	if rec == nil {
		rec = &runRecord{
			info:   RunInfo{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, Status: RunRunning},
			cancel: cancel,
			subs:   make(map[int]SeqSink),
		}
		h.runs[runID] = rec
		h.bySession[sessionID] = runID
		return ctx, nil
	}
	rec.mu.Lock()
	rec.cancel = cancel
	rec.info.Status = RunRunning
	rec.endedAt = time.Time{}
	rec.mu.Unlock()
	h.bySession[sessionID] = runID
	return ctx, nil
}

// publish assigns the next sequence number to env, appends it to the run's
// replay buffer, advances the terminal status for terminal event types, and
// fans the event out to all current subscribers.
func (h *RunHub) publish(runID string, env *protocol.Envelope) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return
	}

	rec.mu.Lock()
	rec.seq++
	item := SeqEnvelope{Seq: rec.seq, Env: env}
	rec.buffer = append(rec.buffer, item)
	if len(rec.buffer) > EventBufferCap {
		rec.buffer = rec.buffer[len(rec.buffer)-EventBufferCap:]
	}
	if st, ok := terminalStatusForEvent(env.Type); ok {
		rec.info.Status = st
	}
	rec.info.LastSeq = rec.seq
	subs := make([]SeqSink, 0, len(rec.subs))
	for _, s := range rec.subs {
		subs = append(subs, s)
	}
	rec.mu.Unlock()

	for _, s := range subs {
		s(item)
	}
}

// Subscribe attaches a plain sink (seq discarded) to the run's live event
// stream. See SubscribeSeq.
func (h *RunHub) Subscribe(runID string, fromSeq int, sink EventSink) (int, bool) {
	return h.SubscribeSeq(runID, fromSeq, func(item SeqEnvelope) { sink(item.Env) })
}

// SubscribeSeq attaches sink to the run's live event stream after replaying
// any buffered events with seq > fromSeq (pass 0 to replay everything
// retained). It returns a subscriber id for Unsubscribe and whether the run
// exists. Events emitted between replay and registration cannot be lost: the
// record lock is held across both.
func (h *RunHub) SubscribeSeq(runID string, fromSeq int, sink SeqSink) (int, bool) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return 0, false
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, item := range rec.buffer {
		if item.Seq > fromSeq {
			sink(item)
		}
	}
	rec.nextSub++
	id := rec.nextSub
	rec.subs[id] = sink
	return id, true
}

// Unsubscribe detaches a subscriber; unknown ids are ignored.
func (h *RunHub) Unsubscribe(runID string, subID int) {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	delete(rec.subs, subID)
	rec.mu.Unlock()
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

// Cancel cancels the run's context, which unwinds the run goroutine. It
// reports whether a live run with that id existed.
func (h *RunHub) Cancel(runID string) bool {
	h.mu.Lock()
	rec := h.runs[runID]
	h.mu.Unlock()
	if rec == nil {
		return false
	}
	rec.cancel()
	return true
}

// setStopHook records the graceful-stop callback for a live run (the run
// goroutine calls this once its StreamedResult exists).
func (h *RunHub) setStopHook(runID string, stop func()) {
	h.mu.Lock()
	if rec := h.runs[runID]; rec != nil {
		rec.stopAfterTurn = stop
	}
	h.mu.Unlock()
}

// StopAfterTurn requests a graceful stop of a live run: the current turn
// finishes and the run ends cleanly before the next one. Reports whether a live
// run with a stop hook existed. Falls back to nothing (returns false) for a run
// that has not yet installed its hook.
func (h *RunHub) StopAfterTurn(runID string) bool {
	h.mu.Lock()
	var stop func()
	if rec := h.runs[runID]; rec != nil {
		stop = rec.stopAfterTurn
	}
	h.mu.Unlock()
	if stop == nil {
		return false
	}
	stop()
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
				}
			}
			h.mu.Unlock()
		}
	}
}
