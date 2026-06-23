package handler

import (
	"encoding/json"
	"sync"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// WSHandler dispatches WebSocket messages to start runs and handle tool approvals and rejections.
type WSHandler struct {
	runner *bridge.Runner

	mu               sync.Mutex
	pendingStates    map[string]*agents.RunState
	pendingSessions  map[string]string
	pendingConfigs   map[string]string
	pendingSandboxes map[string]string
}

// NewWSHandler returns a WebSocket handler backed by the given runner.
func NewWSHandler(runner *bridge.Runner) *WSHandler {
	return &WSHandler{
		runner:           runner,
		pendingStates:    make(map[string]*agents.RunState),
		pendingSessions:  make(map[string]string),
		pendingConfigs:   make(map[string]string),
		pendingSandboxes: make(map[string]string),
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
	var matchedRunID string
	for runID, state := range h.pendingStates {
		for _, item := range state.Interruptions {
			if item.CallID == msg.ToolCallID {
				state.Approve(item, false)
				matchedRunID = runID
			}
		}
	}
	sdkState := h.pendingStates[matchedRunID]
	sessionID := h.pendingSessions[matchedRunID]
	agentConfigID := h.pendingConfigs[matchedRunID]
	sandboxID := h.pendingSandboxes[matchedRunID]
	delete(h.pendingStates, matchedRunID)
	delete(h.pendingSessions, matchedRunID)
	delete(h.pendingConfigs, matchedRunID)
	delete(h.pendingSandboxes, matchedRunID)
	h.mu.Unlock()

	if sdkState == nil {
		log.Error().Str("tool_call_id", msg.ToolCallID).Msg("no pending state for approval")
		return
	}

	result := h.runner.ResumeStreamed(ctx, sdkState, sessionID, agentConfigID, sandboxID, wsSink(conn))
	if result != nil && result.Interrupted {
		h.savePendingState(result)
	}
}

func (h *WSHandler) handleToolReject(conn *server.WSConn, msg protocol.ToolReject) {
	ctx := conn.Context()
	log := zerolog.Ctx(ctx)

	h.mu.Lock()
	var matchedRunID string
	for runID, state := range h.pendingStates {
		for _, item := range state.Interruptions {
			if item.CallID == msg.ToolCallID {
				state.Reject(item, false, msg.Reason)
				matchedRunID = runID
			}
		}
	}
	sdkState := h.pendingStates[matchedRunID]
	sessionID := h.pendingSessions[matchedRunID]
	agentConfigID := h.pendingConfigs[matchedRunID]
	sandboxID := h.pendingSandboxes[matchedRunID]
	delete(h.pendingStates, matchedRunID)
	delete(h.pendingSessions, matchedRunID)
	delete(h.pendingConfigs, matchedRunID)
	delete(h.pendingSandboxes, matchedRunID)
	h.mu.Unlock()

	if sdkState == nil {
		log.Error().Str("tool_call_id", msg.ToolCallID).Msg("no pending state for rejection")
		return
	}

	result := h.runner.ResumeStreamed(ctx, sdkState, sessionID, agentConfigID, sandboxID, wsSink(conn))
	if result != nil && result.Interrupted {
		h.savePendingState(result)
	}
}

func (h *WSHandler) savePendingState(result *bridge.RunResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendingStates[result.RunID] = result.SDKState
	h.pendingSessions[result.RunID] = result.SessionID
	h.pendingConfigs[result.RunID] = result.AgentConfigID
	h.pendingSandboxes[result.RunID] = result.SandboxID
}
