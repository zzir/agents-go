package handler

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

type pendingRun struct {
	state         *agents.RunState
	sessionID     string
	agentConfigID string
	sandboxID     string
	createdAt     time.Time
}

const pendingTTL = 10 * time.Minute

// WSHandler dispatches WebSocket messages to start runs and handle tool approvals and rejections.
type WSHandler struct {
	runner *bridge.Runner

	mu      sync.Mutex
	pending map[string]*pendingRun
}

// NewWSHandler returns a WebSocket handler backed by the given runner.
func NewWSHandler(runner *bridge.Runner) *WSHandler {
	h := &WSHandler{
		runner:  runner,
		pending: make(map[string]*pendingRun),
	}
	go h.cleanupLoop()
	return h
}

func (h *WSHandler) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		h.mu.Lock()
		for id, p := range h.pending {
			if now.Sub(p.createdAt) > pendingTTL {
				delete(h.pending, id)
			}
		}
		h.mu.Unlock()
	}
}

// wsSink returns an event sink that writes envelopes to conn. A write error
// means the client went away mid-run; the run is cancelled when its context is
// done, so the error is not actionable here.
func wsSink(conn *server.WSConn) bridge.EventSink {
	return func(env *protocol.Envelope) {
		_ = conn.WriteJSON(env)
	}
}

// Handle reads and dispatches WebSocket messages on conn until the connection closes.
func (h *WSHandler) Handle(conn *server.WSConn) {
	log := zerolog.Ctx(conn.Context())

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
			go h.handleRunCreate(conn, msg)

		case "tool.approve":
			var msg protocol.ToolApprove
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal tool.approve")
				continue
			}
			go h.handleToolApprove(conn, msg)

		case "tool.reject":
			var msg protocol.ToolReject
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				log.Error().Err(err).Msg("unmarshal tool.reject")
				continue
			}
			go h.handleToolReject(conn, msg)

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

func (h *WSHandler) handleRunCreate(conn *server.WSConn, msg protocol.RunCreate) {
	result := h.runner.RunStreamed(conn.Context(), msg.SessionID, msg.AgentConfigID, msg.SandboxID, msg.Input, wsSink(conn))
	if result == nil {
		return
	}
	if result.Interrupted {
		h.savePendingState(result)
	}
}

func (h *WSHandler) handleToolApprove(conn *server.WSConn, msg protocol.ToolApprove) {
	ctx := conn.Context()
	log := zerolog.Ctx(ctx)

	h.mu.Lock()
	matchedRunID, matchedItem := h.findPendingItem(msg.ToolCallID)
	p := h.pending[matchedRunID]
	delete(h.pending, matchedRunID)
	h.mu.Unlock()

	if p == nil || matchedItem == nil {
		log.Error().Str("tool_call_id", msg.ToolCallID).Msg("no pending state for approval")
		return
	}

	p.state.Approve(matchedItem, false)

	result := h.runner.ResumeStreamed(ctx, p.state, p.sessionID, p.agentConfigID, p.sandboxID, wsSink(conn))
	if result != nil && result.Interrupted {
		h.savePendingState(result)
	}
}

func (h *WSHandler) handleToolReject(conn *server.WSConn, msg protocol.ToolReject) {
	ctx := conn.Context()
	log := zerolog.Ctx(ctx)

	h.mu.Lock()
	matchedRunID, matchedItem := h.findPendingItem(msg.ToolCallID)
	p := h.pending[matchedRunID]
	delete(h.pending, matchedRunID)
	h.mu.Unlock()

	if p == nil || matchedItem == nil {
		log.Error().Str("tool_call_id", msg.ToolCallID).Msg("no pending state for rejection")
		return
	}

	p.state.Reject(matchedItem, false, msg.Reason)

	result := h.runner.ResumeStreamed(ctx, p.state, p.sessionID, p.agentConfigID, p.sandboxID, wsSink(conn))
	if result != nil && result.Interrupted {
		h.savePendingState(result)
	}
}

// findPendingItem locates the pending run and interruption matching callID.
// Must be called with h.mu held.
func (h *WSHandler) findPendingItem(callID string) (string, *agents.ToolApprovalItem) {
	for runID, p := range h.pending {
		for _, item := range p.state.Interruptions {
			if item.CallID == callID {
				return runID, item
			}
		}
	}
	return "", nil
}

func (h *WSHandler) savePendingState(result *bridge.RunResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending[result.RunID] = &pendingRun{
		state:         result.SDKState,
		sessionID:     result.SessionID,
		agentConfigID: result.AgentConfigID,
		sandboxID:     result.SandboxID,
		createdAt:     time.Now(),
	}
}
