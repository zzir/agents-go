package bridge

import (
	"context"
	"hash/fnv"
	"slices"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// PendingCall is one decision a person is waited on for.
type PendingCall struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	// Kind is "step" for a workflow step waiting to start, empty for a tool call.
	Kind  string `json:"kind,omitempty"`
	RunID string `json:"run_id"`
	// SessionID is the conversation to open; a task's call names its parent conversation.
	SessionID string    `json:"session_id"`
	TaskID    string    `json:"task_id,omitempty"`
	TaskLabel string    `json:"task_label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is when the wait ends unanswered; absent when approvals do not expire.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// SessionState is one conversation's derived status and what it waits on.
type SessionState struct {
	Status    string
	LiveRunID string
	Pending   []PendingCall
}

// Wire is the state as GET /sessions and session.status carry it.
func (s SessionState) Wire(sessionID string) protocol.SessionStatus {
	out := protocol.SessionStatus{
		SessionID: sessionID, Status: s.Status, LiveRunID: s.LiveRunID, PendingCount: len(s.Pending),
	}
	if len(s.Pending) > 0 {
		out.OldestPendingAt = &s.Pending[0].CreatedAt
	}
	return out
}

// sessionSignals is what a status is derived from, per conversation.
type sessionSignals struct {
	pending                  []PendingCall
	waiting, working, failed bool
}

// SessionStates answers the status of any conversation SessionStatuses covered.
type SessionStates struct {
	signals map[string]*sessionSignals
	live    map[string]string
}

// Of derives one conversation's state, highest priority first: a decision
// waited on, live work, a run that ended in error, idle.
func (s SessionStates) Of(sessionID string) SessionState {
	sig := s.signals[sessionID]
	if sig == nil {
		sig = &sessionSignals{}
	}
	out := SessionState{LiveRunID: s.live[sessionID], Pending: sig.pending}
	switch {
	case len(sig.pending) > 0 || sig.waiting:
		out.Status = protocol.SessionRequiresAction
	case out.LiveRunID != "" || sig.working:
		out.Status = protocol.SessionRunning
	case sig.failed:
		out.Status = protocol.SessionFailed
	default:
		out.Status = protocol.SessionIdle
	}
	return out
}

// Pending lists every decision the covered conversations wait on, oldest first.
func (s SessionStates) Pending() []PendingCall {
	out := []PendingCall{}
	for _, sig := range s.signals {
		out = append(out, sig.pending...)
	}
	slices.SortStableFunc(out, func(a, b PendingCall) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// SessionStatuses is the one derivation of a conversation's status, for
// ownerID's visible sessions (among ids when given) — invariant 3.
func (r *Runner) SessionStatuses(ctx context.Context, ownerID string, ids []string) (SessionStates, error) {
	states := SessionStates{signals: map[string]*sessionSignals{}, live: r.hub.liveBySession()}
	of := func(id string) *sessionSignals {
		if states.signals[id] == nil {
			states.signals[id] = &sessionSignals{}
		}
		return states.signals[id]
	}
	entries := store.NewSharedEntryStore(r.db)

	if r.Deps.PendingApprovals != nil {
		open, err := r.Deps.PendingApprovals.ListOpen(ctx, ownerID, ids)
		if err != nil {
			return states, err
		}
		var ttl time.Duration
		if r.Deps.Settings != nil {
			ttl = time.Duration(r.Deps.Settings.Int(ctx, settings.KeyApprovalTTLMinutes)) * time.Minute
		}
		offPath := map[string]map[string]bool{}
		for i := range open {
			a := &open[i]
			// A pause whose run was branched away is out of view — invariant 19.
			if a.TaskID == "" {
				off, read := offPath[a.SessionID]
				if !read {
					ref, err := store.RefFor(ctx, r.db, a.SessionID)
					if err != nil {
						return states, err
					}
					if off, err = entries.RunsOffPath(ctx, ref); err != nil {
						return states, err
					}
					offPath[a.SessionID] = off
				}
				if off[a.RunID] {
					continue
				}
			}
			sig := of(a.OpenSessionID)
			for _, c := range a.Calls() {
				call := PendingCall{
					ToolCallID: c.ToolCallID, ToolName: c.ToolName, Kind: a.Kind, RunID: a.RunID,
					SessionID: a.OpenSessionID, TaskID: a.TaskID, TaskLabel: a.TaskLabel, CreatedAt: a.CreatedAt,
				}
				if ttl > 0 {
					at := a.CreatedAt.Add(ttl)
					call.ExpiresAt = &at
				}
				sig.pending = append(sig.pending, call)
			}
		}
	}
	if r.Deps.Tasks != nil {
		rows, err := r.Deps.Tasks.LiveSignals(ctx, ownerID, ids)
		if err != nil {
			return states, err
		}
		for _, t := range rows {
			switch t.Status {
			case protocol.TaskInputRequired:
				of(t.ParentSessionID).waiting = true
			case protocol.TaskWorking:
				of(t.ParentSessionID).working = true
			}
		}
	}
	failed, err := entries.EndedInError(ctx, ownerID, ids)
	if err != nil {
		return states, err
	}
	for id := range failed {
		of(id).failed = true
	}
	return states, nil
}

// LiveRun reports the conversation's own executing run; a paused run is not live.
func (r *Runner) LiveRun(sessionID string) (string, bool) {
	return r.hub.ActiveRunForSession(sessionID)
}

// statusStripes bounds the locks that keep one conversation's status
// broadcasts in the order they were derived.
const statusStripes = 32

// PublishSessionStatus derives the conversation's status and broadcasts it to
// its owner's connections; a task's hidden session reports its parent.
func (r *Runner) PublishSessionStatus(ctx context.Context, sessionID string) {
	// A server shutting down has nobody to tell, and soon no database to ask.
	if r.OnBroadcast == nil || sessionID == "" || r.hub.rootCtx.Err() != nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if meta, err := r.taskMeta(ctx, sessionID); err == nil && meta != nil && meta.ParentSessionID != "" {
		sessionID = meta.ParentSessionID
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	mu := &r.statusMu[h.Sum32()%statusStripes]
	mu.Lock()
	defer mu.Unlock()
	states, err := r.SessionStatuses(ctx, store.EveryOwner, []string{sessionID})
	if err != nil {
		logging.Ctx(ctx).Warn("deriving session status", "error", err, "session_id", sessionID)
		return
	}
	env, err := protocol.NewEnvelope(protocol.EventSessionStatus, states.Of(sessionID).Wire(sessionID))
	if err != nil {
		return
	}
	r.OnBroadcast(ctx, env, "", sessionID)
}
