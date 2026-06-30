package bridge

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

type wsRunHooks struct {
	agents.BaseRunHooks
	send      func(string, any)
	traces    *store.TraceStore
	sessionID string
	runID     string
}

func newWSRunHooks(send func(string, any), traces *store.TraceStore, sessionID, runID string) *wsRunHooks {
	return &wsRunHooks{
		send:      send,
		traces:    traces,
		sessionID: sessionID,
		runID:     runID,
	}
}

// Emit sends and persists an arbitrary hook event. Used by CompactionAdapter
// so compaction trace events go through the same path as SDK hook events.
func (h *wsRunHooks) Emit(ev protocol.HookEvent) {
	ev.RunID = h.runID
	h.send("hook.event", ev)
	h.persist(ev)
}

func (h *wsRunHooks) persist(ev protocol.HookEvent) {
	if h.traces == nil {
		return
	}
	detail := ev.Detail
	if ev.ToolName != "" && detail == "" {
		detail = ev.ToolName
	}
	if ev.From != "" && ev.To != "" && detail == "" {
		detail = ev.From + " → " + ev.To
	}
	te := &store.TraceEvent{
		SessionID: h.sessionID,
		RunID:     h.runID,
		Kind:      "hook",
		Name:      ev.Hook,
		Detail:    detail,
		Data:      marshalHookData(ev),
	}
	if err := h.traces.Insert(context.Background(), te); err != nil {
		log.Warn().Err(err).Str("hook", ev.Hook).Msg("failed to persist hook event")
	}
}

func marshalHookData(ev protocol.HookEvent) string {
	parts := ""
	if ev.AgentName != "" {
		parts += "agent=" + ev.AgentName
	}
	if ev.ToolName != "" {
		if parts != "" {
			parts += " "
		}
		parts += "tool=" + ev.ToolName
	}
	if ev.From != "" {
		if parts != "" {
			parts += " "
		}
		parts += "from=" + ev.From
	}
	if ev.To != "" {
		if parts != "" {
			parts += " "
		}
		parts += "to=" + ev.To
	}
	return parts
}

func (h *wsRunHooks) OnAgentStart(_ context.Context, _ *agents.RunContext, agent *agents.Agent) error {
	ev := protocol.HookEvent{RunID: h.runID, Hook: "agent_start", AgentName: agent.Name}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}

func (h *wsRunHooks) OnAgentEnd(_ context.Context, _ *agents.RunContext, agent *agents.Agent, output any) error {
	ev := protocol.HookEvent{RunID: h.runID, Hook: "agent_end", AgentName: agent.Name, Detail: fmt.Sprintf("%v", output)}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}

func (h *wsRunHooks) OnHandoff(_ context.Context, _ *agents.RunContext, from, to *agents.Agent) error {
	ev := protocol.HookEvent{RunID: h.runID, Hook: "handoff", From: from.Name, To: to.Name}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}

func (h *wsRunHooks) OnToolStart(_ context.Context, _ *agents.RunContext, agent *agents.Agent, tool agents.Tool) error {
	ev := protocol.HookEvent{RunID: h.runID, Hook: "tool_start", AgentName: agent.Name, ToolName: tool.ToolName()}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}

func (h *wsRunHooks) OnToolEnd(_ context.Context, _ *agents.RunContext, agent *agents.Agent, tool agents.Tool, _ any) error {
	ev := protocol.HookEvent{RunID: h.runID, Hook: "tool_end", AgentName: agent.Name, ToolName: tool.ToolName()}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}

func (h *wsRunHooks) OnLLMStart(_ context.Context, _ *agents.RunContext, agent *agents.Agent, _ string, _ []agents.TResponseInputItem) error {
	ev := protocol.HookEvent{RunID: h.runID, Hook: "llm_start", AgentName: agent.Name}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}

func (h *wsRunHooks) OnLLMEnd(_ context.Context, _ *agents.RunContext, agent *agents.Agent, resp *agents.ModelResponse) error {
	detail := ""
	if resp != nil && resp.Usage != nil {
		detail = fmt.Sprintf("input=%d output=%d", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	ev := protocol.HookEvent{RunID: h.runID, Hook: "llm_end", AgentName: agent.Name, Detail: detail}
	h.send("hook.event", ev)
	h.persist(ev)
	return nil
}
