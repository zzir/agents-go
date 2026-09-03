package handler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// WSHandler dispatches WebSocket messages to start runs and handle tool
// approvals. Runs live in the runner's hub, independent of the connection,
// and their events are a broadcast bus (invariant 14); approvals are
// persisted by the runner, so they work across reconnects and restarts.
type WSHandler struct {
	runner    *bridge.Runner
	registry  *ConnRegistry
	sessions  *store.SessionStore
	approvals *store.PendingApprovalStore
	// Audit, when set, records the acts that bypass REST: run starts and
	// approval decisions made over the socket. Wired at bootstrap.
	Audit protocol.AuditFunc
}

// audit records one explicit event for conn's user; a nil Audit is silence.
func (h *WSHandler) audit(conn *server.WSConn, action, resource, detail string) {
	if h.Audit == nil {
		return
	}
	h.Audit(context.WithoutCancel(conn.Context()), protocol.AuditRecord{
		Actor: conn.User, Action: action, Resource: resource, Detail: detail,
	})
}

// NewWSHandler returns a WebSocket handler backed by the runner and wires the
// runner's attach hook to the connection registry. The hook is a plain field
// the run goroutines read, so this must run before anything can start a run.
func NewWSHandler(runner *bridge.Runner, sessions *store.SessionStore, approvals *store.PendingApprovalStore) *WSHandler {
	h := &WSHandler{runner: runner, registry: NewConnRegistry(runner.Hub(), sessions), sessions: sessions, approvals: approvals}
	runner.OnRunAttach = h.registry.AttachAll
	runner.OnBroadcast = h.registry.Broadcast
	return h
}

// ownsRun reports whether conn's user owns the session of a live run.
func (h *WSHandler) ownsRun(conn *server.WSConn, runID string) bool {
	info, ok := h.runner.Hub().Info(runID)
	return ok && info.OwnerID == conn.User.ID
}

// refuseRun tells the client a run is not theirs to touch — indistinguishable
// from one that does not exist.
func refuseRun(conn *server.WSConn, runID string) {
	_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
		RunID: runID, Code: protocol.CodeRunNotFound, Message: "run not found or expired",
	})})
}

// wsSink returns an event sink onto the connection's bounded outbound queue.
// It never blocks the producer: a full queue closes the connection instead.
func wsSink(conn *server.WSConn) bridge.EventSink {
	return func(env *protocol.Envelope) {
		if !conn.WriteAsync(env) {
			conn.Close()
		}
	}
}

// connSubs tracks a connection's live hub subscriptions (the cancel closures
// SubscribeSeq returned) so they can all be detached when the socket closes.
type connSubs struct {
	mu     sync.Mutex
	subs   map[string]func() // runID -> detach
	closed bool
}

// add records the subscription for runID, detaching a previous one to the
// same run first; after closeAll it detaches on the spot (AttachAll snapshots the registry).
func (cs *connSubs) add(runID string, cancel func()) {
	cs.mu.Lock()
	if cs.closed {
		cs.mu.Unlock()
		cancel()
		return
	}
	prev := cs.subs[runID]
	cs.subs[runID] = cancel
	cs.mu.Unlock()
	if prev != nil {
		prev()
	}
}

// has reports whether the connection already subscribes to runID.
func (cs *connSubs) has(runID string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	_, ok := cs.subs[runID]
	return ok
}

// closeAll detaches every subscription and closes the set, so a subscribe
// still in flight detaches itself rather than outliving the connection.
func (cs *connSubs) closeAll() {
	cs.mu.Lock()
	subs := cs.subs
	cs.subs, cs.closed = map[string]func(){}, true
	cs.mu.Unlock()
	// Outside the lock: detaching reaches into the hub, and add already
	// cancels its predecessor unlocked.
	for _, cancel := range subs {
		cancel()
	}
}

// Handle reads and dispatches WebSocket messages on conn until the connection closes.
func (h *WSHandler) Handle(conn *server.WSConn) {
	log := logging.Ctx(conn.Context())
	// Drain outbound events through a bounded queue + writer goroutine so a
	// slow client can't back-pressure the hub/run goroutines that publish them.
	conn.StartWriter()
	subs := &connSubs{subs: map[string]func(){}}
	defer subs.closeAll()
	// Join the broadcast bus: attach to every in-flight run (with replay) now,
	// and let AttachAll pick this connection up for runs started later.
	h.registry.register(conn, subs)
	defer h.registry.unregister(conn)

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			if server.IsNormalClose(err) {
				// Client went away (tab closed, reload, navigate) — expected,
				// not an error.
				log.Debug("ws connection closed")
			} else {
				log.Debug("ws read error", "error", err)
			}
			return
		}

		// The credential is re-checked before each frame: a revoked token or
		// a changed role ends the connection here, not at the next reconnect.
		if !conn.Recheck() {
			return
		}

		switch env.Type {
		case protocol.EventRunCreate:
			var msg protocol.RunCreate
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error("unmarshal run.create", "error", err)
				continue
			}
			h.handleRunCreate(conn, msg)

		case protocol.EventRunSubscribe:
			var msg protocol.RunSubscribe
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error("unmarshal run.subscribe", "error", err)
				continue
			}
			h.subscribe(conn, subs, msg.RunID, msg.FromSeq)

		case protocol.EventToolApprove:
			var msg protocol.ToolApprove
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error("unmarshal tool.approve", "error", err)
				continue
			}
			go h.resolve(conn, msg.ToolCallID, true, bridge.ParseApprovalScope(msg.Scope), "")

		case protocol.EventToolReject:
			var msg protocol.ToolReject
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error("unmarshal tool.reject", "error", err)
				continue
			}
			go h.resolve(conn, msg.ToolCallID, false, bridge.ApprovalOnce, msg.Reason)

		case protocol.EventRunCancel:
			var msg protocol.RunCancel
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error("unmarshal run.cancel", "error", err)
				continue
			}
			if !h.ownsRun(conn, msg.RunID) {
				refuseRun(conn, msg.RunID)
				continue
			}
			if msg.Mode == "graceful" {
				h.runner.StopRunAfterTurn(msg.RunID)
			} else {
				h.runner.CancelRun(msg.RunID)
			}

		case protocol.EventRunInject:
			var msg protocol.RunInject
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error("unmarshal run injection", "error", err, "type", env.Type)
				continue
			}
			h.inject(conn, msg)

		default:
			log.Warn("unknown ws message type", "type", env.Type)
		}
	}
}

// subscribe attaches conn to runID's event stream (replaying from fromSeq),
// tracking the subscription on subs for cleanup.
func (h *WSHandler) subscribe(conn *server.WSConn, subs *connSubs, runID string, fromSeq int) {
	if !h.ownsRun(conn, runID) {
		refuseRun(conn, runID)
		return
	}
	cancel, ok := h.runner.Hub().Subscribe(runID, fromSeq, wsSink(conn))
	if !ok {
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
			RunID: runID, Code: protocol.CodeRunNotFound, Message: "run not found or expired",
		})})
		return
	}
	subs.add(runID, cancel)
}

func (h *WSHandler) handleRunCreate(conn *server.WSConn, msg protocol.RunCreate) {
	// Only the session's owner starts runs in it; a foreign session reads as
	// absent, the same as a missing one.
	if sess, err := h.sessions.Get(conn.Context(), msg.SessionID); err != nil || sess.OwnerID != conn.User.ID {
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
			SessionID: msg.SessionID, Code: protocol.CodeSessionNotFound, Message: "session not found: " + msg.SessionID,
		})})
		return
	}
	// No explicit subscribe: OnRunAttach attached every connection of the
	// owner before the first event. StartRun applies the plan intent inside its reservation.
	_, err := h.runner.StartRun(msg.SessionID, msg.AgentConfigID, msg.ProjectID, bridge.RunInput{Text: msg.Input, AttachmentIDs: msg.AttachmentIDs}, msg.Plan, nil)
	if err == nil {
		h.audit(conn, "ws.run.create", msg.SessionID, "")
	}
	if err != nil {
		// These fire before any run.started, so carry the session id; classify
		// like the REST path (busy/limit/deleting are session-busy conflicts).
		busy, isBusy := errors.AsType[bridge.ErrSessionBusy](err)
		_, atTaskLimit := errors.AsType[bridge.ErrTaskLimit](err)
		_, deleting := errors.AsType[bridge.ErrSessionDeleting](err)
		_, draining := errors.AsType[bridge.ErrShuttingDown](err)
		var runID string
		code := protocol.CodeConfigError // a genuine server-side failure by default
		switch {
		case isBusy:
			code, runID = protocol.CodeSessionBusy, busy.RunID
		case atTaskLimit, deleting, draining:
			code = protocol.CodeSessionBusy
		case errors.Is(err, store.ErrNotFound):
			code = protocol.CodeSessionNotFound
		}
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
			RunID: runID, SessionID: msg.SessionID, Code: code, Message: err.Error(),
		})})
		return
	}
}

// resolve applies an approve/reject decision (persisted by the runner). The
// decision resumes the SAME run id; OnRunAttach re-attaches connections not yet watching.
func (h *WSHandler) resolve(conn *server.WSConn, toolCallID string, approve bool, scope bridge.ApprovalScope, reason string) {
	log := logging.Ctx(conn.Context())
	pending, ok := ownsApproval(conn.Context(), h.approvals, h.sessions, conn.User.ID, toolCallID)
	if !ok {
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
			Code: protocol.CodeApprovalFailed, Message: "approval not found",
		})})
		return
	}
	_, sessionID, err := h.runner.ResolveApproval(conn.Context(), toolCallID, approve, scope, reason, nil)
	if err == nil {
		h.audit(conn, "ws.approval", toolCallID, auditDecision(approve, scope, pending))
	}
	if err != nil {
		log.Error("resolve approval failed", "error", err, "tool_call_id", toolCallID)
		// Carry the session id (when known) so the client can rebuild the
		// paused turn's approval card: the resume never happened.
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
			SessionID: sessionID, Code: protocol.CodeApprovalFailed, Message: err.Error(),
		})})
		return
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// inject delivers client input to a live run through the queue the message
// names. A run that has already finished is reported, not ignored.
func (h *WSHandler) inject(conn *server.WSConn, msg protocol.RunInject) {
	if !h.ownsRun(conn, msg.RunID) {
		refuseRun(conn, msg.RunID)
		return
	}
	delivered, err := h.runner.Hub().Inject(msg.RunID, msg.Queue, msg.Input)
	if err != nil {
		logging.Ctx(conn.Context()).Error("run injection", "error", err, "queue", msg.Queue)
	}
	if delivered && err == nil {
		return
	}
	_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventRunError, Payload: mustJSON(protocol.RunError{
		RunID:   msg.RunID,
		Code:    protocol.CodeRunNotFound,
		Message: "the run is no longer accepting input",
	})})
}
