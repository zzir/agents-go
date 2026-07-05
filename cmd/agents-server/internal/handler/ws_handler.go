package handler

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// WSHandler dispatches WebSocket messages to start runs and handle tool
// approvals and rejections. Runs live in the runner's hub, independent of the
// connection; the handler subscribes the connection to a run's event stream
// and cleans its subscriptions up on disconnect. Approvals are persisted by
// the runner, so approve/reject work across reconnects and restarts.
type WSHandler struct {
	runner *bridge.Runner
}

// NewWSHandler returns a WebSocket handler backed by the given runner.
func NewWSHandler(runner *bridge.Runner) *WSHandler {
	return &WSHandler{runner: runner}
}

// wsSink returns an event sink that writes envelopes to conn. A write error
// means the client went away; the run keeps executing in the hub and can be
// resubscribed after reconnect, so the error is not actionable here.
func wsSink(conn *server.WSConn) bridge.EventSink {
	return func(env *protocol.Envelope) {
		_ = conn.WriteJSON(env)
	}
}

// connSubs tracks a connection's live hub subscriptions so they can all be
// detached when the socket closes.
type connSubs struct {
	hub  *bridge.RunHub
	mu   sync.Mutex
	subs map[string]int // runID -> subscriber id
}

func (cs *connSubs) add(runID string, subID int) {
	cs.mu.Lock()
	cs.subs[runID] = subID
	cs.mu.Unlock()
}

func (cs *connSubs) closeAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for runID, subID := range cs.subs {
		cs.hub.Unsubscribe(runID, subID)
	}
	cs.subs = map[string]int{}
}

// Handle reads and dispatches WebSocket messages on conn until the connection closes.
func (h *WSHandler) Handle(conn *server.WSConn) {
	log := zerolog.Ctx(conn.Context())
	subs := &connSubs{hub: h.runner.Hub(), subs: map[string]int{}}
	defer subs.closeAll()

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			log.Debug().Err(err).Msg("ws read error")
			return
		}

		switch env.Type {
		case "run.create":
			var msg protocol.RunCreate
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal run.create")
				continue
			}
			h.handleRunCreate(conn, subs, msg)

		case "run.subscribe":
			var msg protocol.RunSubscribe
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal run.subscribe")
				continue
			}
			h.subscribe(conn, subs, msg.RunID, msg.FromSeq)

		case "tool.approve":
			var msg protocol.ToolApprove
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal tool.approve")
				continue
			}
			go h.resolve(conn, subs, msg.ToolCallID, true, "")

		case "tool.reject":
			var msg protocol.ToolReject
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal tool.reject")
				continue
			}
			go h.resolve(conn, subs, msg.ToolCallID, false, msg.Reason)

		case "run.cancel":
			var msg protocol.RunCancel
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal run.cancel")
				continue
			}
			h.runner.CancelRun(msg.RunID)

		default:
			log.Warn().Str("type", env.Type).Msg("unknown ws message type")
		}
	}
}

// subscribe attaches conn to runID's event stream (replaying from fromSeq),
// tracking the subscription on subs for cleanup.
func (h *WSHandler) subscribe(conn *server.WSConn, subs *connSubs, runID string, fromSeq int) {
	subID, ok := h.runner.Hub().Subscribe(runID, fromSeq, wsSink(conn))
	if !ok {
		_ = conn.WriteJSON(&protocol.Envelope{Type: "run.error", Payload: mustJSON(protocol.RunError{
			RunID: runID, Code: "run_not_found", Message: "run not found or expired",
		})})
		return
	}
	subs.add(runID, subID)
}

func (h *WSHandler) handleRunCreate(conn *server.WSConn, subs *connSubs, msg protocol.RunCreate) {
	runID, err := h.runner.StartRun(msg.SessionID, msg.AgentConfigID, msg.SandboxID, msg.Input, nil)
	if err != nil {
		// These fire before any run.started, so no run→session mapping exists
		// client-side yet: carry the session id so the error is attributable.
		var busy bridge.ErrSessionBusy
		if errors.As(err, &busy) {
			_ = conn.WriteJSON(&protocol.Envelope{Type: "run.error", Payload: mustJSON(protocol.RunError{
				RunID: busy.RunID, SessionID: msg.SessionID, Code: "session_busy", Message: "session already has an active run",
			})})
			return
		}
		_ = conn.WriteJSON(&protocol.Envelope{Type: "run.error", Payload: mustJSON(protocol.RunError{
			SessionID: msg.SessionID, Code: "session_not_found", Message: err.Error(),
		})})
		return
	}
	// Subscribe from 0 so the run.started already buffered is delivered.
	h.subscribe(conn, subs, runID, 0)
}

// resolve applies an approve/reject decision (persisted by the runner) and
// subscribes conn to the resumed run.
func (h *WSHandler) resolve(conn *server.WSConn, subs *connSubs, toolCallID string, approve bool, reason string) {
	log := zerolog.Ctx(conn.Context())
	runID, err := h.runner.ResolveApproval(conn.Context(), toolCallID, approve, reason, nil)
	if err != nil {
		log.Error().Err(err).Str("tool_call_id", toolCallID).Msg("resolve approval failed")
		_ = conn.WriteJSON(&protocol.Envelope{Type: "run.error", Payload: mustJSON(protocol.RunError{
			Code: "approval_failed", Message: err.Error(),
		})})
		return
	}
	h.subscribe(conn, subs, runID, 0)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
