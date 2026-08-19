package handler

import (
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
// approvals and rejections. Runs live in the runner's hub, independent of the
// connection, and their events are a broadcast bus: the registry attaches
// every connection to every run (on connect, and via Runner.OnRunAttach when
// a run starts or resumes), so a second browser on the same session streams
// the conversation live instead of waiting for a reload. Approvals are
// persisted by the runner, so approve/reject work across reconnects and
// restarts.
type WSHandler struct {
	runner   *bridge.Runner
	registry *ConnRegistry
}

// NewWSHandler returns a WebSocket handler backed by the given runner and
// wires the runner's attach hook to the connection registry.
//
// The hook is a plain field the run goroutines read, so this must run before
// anything that can start a run — the caller orders the startup sweeps around
// it (see cmd.run).
func NewWSHandler(runner *bridge.Runner) *WSHandler {
	h := &WSHandler{runner: runner, registry: NewConnRegistry(runner.Hub())}
	runner.OnRunAttach = h.registry.AttachAll
	runner.OnBroadcast = h.registry.Broadcast
	return h
}

// wsSink returns an event sink that enqueues envelopes onto the connection's
// bounded outbound queue. It never blocks the producer (the hub / run
// goroutine): if the queue is full, the client is genuinely stuck, so the
// connection is closed — the run keeps executing in the hub and the client
// resubscribes and replays after reconnecting.
func wsSink(conn *server.WSConn) bridge.EventSink {
	return func(env *protocol.Envelope) {
		if !conn.WriteAsync(env) {
			conn.Close()
		}
	}
}

// connSubs tracks a connection's live hub subscriptions — each held as the
// cancel closure SubscribeSeq returned — so they can all be detached when the
// socket closes.
type connSubs struct {
	mu   sync.Mutex
	subs map[string]func() // runID -> detach
}

// add records the connection's subscription for runID, detaching any previous
// subscription to the same run first so re-subscribing (reconnect with a new
// from_seq, approval resume) never leaves a duplicate hub sink delivering
// every event twice.
func (cs *connSubs) add(runID string, cancel func()) {
	cs.mu.Lock()
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

func (cs *connSubs) closeAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, cancel := range cs.subs {
		cancel()
	}
	cs.subs = map[string]func(){}
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
	// No explicit subscribe here: the runner's OnRunAttach hook attached every
	// connection (this one included) before the first event published. The plan
	// intent rides the request: StartRun applies it inside the reservation, so a
	// busy refusal never mutates the session's phase.
	_, err := h.runner.StartRun(msg.SessionID, msg.AgentConfigID, msg.SandboxID, msg.WorkDir, msg.Input, msg.Plan, nil)
	if err != nil {
		// These fire before any run.started, so no run→session mapping exists
		// client-side yet: carry the session id so the error is attributable.
		// Classify like the REST path instead of labeling everything
		// session_not_found: a DB failure or a delete-in-progress is not a missing
		// session, and busy/limit/deleting are conflicts the client should treat
		// like session-busy (drop the optimistic bubble; the run never started).
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
// decision resumes the SAME run id; the runner's OnRunAttach hook re-attaches
// any connection not already watching it (a connection that watched the
// interrupted run is still attached and just keeps receiving events).
func (h *WSHandler) resolve(conn *server.WSConn, toolCallID string, approve bool, scope bridge.ApprovalScope, reason string) {
	log := logging.Ctx(conn.Context())
	_, sessionID, err := h.runner.ResolveApproval(conn.Context(), toolCallID, approve, scope, reason, nil)
	if err != nil {
		log.Error("resolve approval failed", "error", err, "tool_call_id", toolCallID)
		// Carry the session id (when known) so the client can rebuild the paused
		// turn's approval card — the optimistic approve/reject status was applied
		// but the resume never happened.
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
// names.
//
// A run that has already finished is reported rather than ignored: the user
// typed something and it went nowhere, which they need to know — the client
// turns it into a new run or shows it as undelivered.
func (h *WSHandler) inject(conn *server.WSConn, msg protocol.RunInject) {
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
