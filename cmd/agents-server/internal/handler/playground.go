package handler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// PlaygroundHandler serves one-off model calls for replaying a generation
// from the trace panel with edited inputs. Calls go straight to the model —
// no session history is read or written and no run is recorded.
type PlaygroundHandler struct {
	deps *bridge.AgentDeps
}

// NewPlaygroundHandler returns a handler backed by the given agent dependencies.
func NewPlaygroundHandler(deps *bridge.AgentDeps) *PlaygroundHandler {
	return &PlaygroundHandler{deps: deps}
}

type playgroundReq struct {
	// AgentConfigID selects whose provider credentials and default model to use.
	AgentConfigID string `json:"agent_config_id"`
	// Model optionally overrides the agent's model.
	Model string `json:"model,omitempty"`
	// SystemInstructions is the system prompt to send.
	SystemInstructions string `json:"system_instructions,omitempty"`
	// InputItems are Responses-format input items, as edited by the user.
	InputItems []json.RawMessage `json:"input_items"`
	// ModelSettings, when set, replaces the agent's configured settings — the
	// replay dialog seeds it from the traced request so edits take effect.
	ModelSettings *agents.ModelSettings `json:"model_settings,omitempty"`
	// Tools are schema-only tool definitions echoed back from the traced
	// request, so the model sees the same tool surface and can emit function
	// calls. They are never executed: a single Respond runs no tool loop.
	Tools []playgroundTool `json:"tools,omitempty"`
	// OutputSchema, when set, requests structured output — echoed from the
	// traced generation so a structured call replays as one. Without it a
	// replay of such a call is a different request (free text).
	OutputSchema *playgroundSchema `json:"output_schema,omitempty"`
	// Stream selects the SSE response: `delta`/`reasoning` text events as they
	// arrive, then one `done` (output, usage, duration_ms, ttft_ms) or `error`.
	Stream bool `json:"stream,omitempty"`
}

type playgroundTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// playgroundSchema mirrors the generation span's output_schema record.
type playgroundSchema struct {
	Name   string         `json:"name,omitempty"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict,omitempty"`
}

type playgroundResp struct {
	Output     json.RawMessage `json:"output"`
	Usage      *agents.Usage   `json:"usage,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	// TTFTMS is the time to first streamed token; -1 on the non-streaming path.
	TTFTMS int64 `json:"ttft_ms,omitempty"`
}

// Generate performs a single non-streaming model call with the given
// instructions and input items, using the agent's provider configuration.
//
//	@Summary		Playground generate
//	@Description	One-off model call for replaying a traced generation with edited inputs. Touches no session, records no run; tools are schema-only and never executed. output_schema replays structured output; stream=true switches the response to SSE (delta/reasoning events, then done or error).
//	@Tags			playground
//	@Accept			json
//	@Produce		json
//	@Param			request	body		playgroundReq	true	"Generation request"
//	@Success		200		{object}	playgroundResp
//	@Failure		400		{object}	ErrorResponse
//	@Failure		502		{object}	ErrorResponse	"model call failed"
//	@Security		BearerAuth
//	@Router			/playground/generate [post]
func (h *PlaygroundHandler) Generate(c *gin.Context) {
	var req playgroundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.AgentConfigID == "" {
		badRequest(c, "agent_config_id is required")
		return
	}

	built, err := bridge.BuildFullAgent(c.Request.Context(), h.deps, req.AgentConfigID, "")
	if err != nil {
		badRequest(c, "building agent: "+err.Error())
		return
	}
	if built.Provider == nil {
		badRequest(c, "no API key configured for this agent")
		return
	}
	modelName := req.Model
	modelName = cmp.Or(modelName, built.Agent.Model)
	model, err := built.Provider.Model(modelName)
	if err != nil || model == nil {
		badRequest(c, "resolving model: "+errString(err))
		return
	}

	items := make([]agents.InputItem, 0, len(req.InputItems))
	for i, raw := range req.InputItems {
		item, err := session.UnmarshalInputItem(store.NormalizeItemJSON(raw))
		if err != nil {
			badRequest(c, "input item "+strconv.Itoa(i)+": "+err.Error())
			return
		}
		items = append(items, item)
	}

	settings := built.Agent.ModelSettings
	if req.ModelSettings != nil {
		settings = req.ModelSettings
	}
	var tools []*agents.Tool
	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		tools = append(tools, &agents.Tool{
			Name:             t.Name,
			Description:      t.Description,
			ParamsJSONSchema: t.Parameters,
			OnInvoke: func(context.Context, *agents.ToolContext, string) (agents.ToolResult, error) {
				return agents.ToolResult{}, errors.New("playground tools are not executable")
			},
		})
	}

	var outputSchema agents.OutputSchema
	if req.OutputSchema != nil && req.OutputSchema.Schema != nil {
		outputSchema = agents.NewDynamicOutputSchema(
			cmp.Or(req.OutputSchema.Name, "final_output"), req.OutputSchema.Schema, req.OutputSchema.Strict)
	}

	mreq := agents.ModelRequest{
		SystemInstructions: req.SystemInstructions,
		Input:              items,
		Settings:           settings,
		Tools:              tools,
		OutputSchema:       outputSchema,
	}

	if req.Stream {
		h.generateStream(c, model, mreq)
		return
	}

	start := time.Now()
	resp, err := model.Respond(c.Request.Context(), mreq)
	if err != nil {
		upstreamError(c, err)
		return
	}

	out, err := json.Marshal(resp.Output)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, playgroundResp{
		Output:     out,
		Usage:      resp.Usage,
		DurationMS: time.Since(start).Milliseconds(),
		TTFTMS:     -1,
	})
}

// generateStream is the SSE variant: text/reasoning deltas as they arrive,
// then one `done` event (output, usage, duration_ms, ttft_ms) or `error`. The
// client cancels by aborting the request — the context tears the model call
// down.
func (h *PlaygroundHandler) generateStream(c *gin.Context, model agents.Model, mreq agents.ModelRequest) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)
	writeEvent := func(event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := c.Writer.WriteString("event: " + event + "\ndata: " + string(data) + "\n\n"); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	start := time.Now()
	var ttft int64 = -1
	var final []agents.OutputItem
	var usage *agents.Usage
	terminal := false
	// Fallback assembly from item.done events: some backends (e.g. ChatGPT
	// with store=false) return an empty Output in the completed event.
	var acc []agents.OutputItem
	// TTFT stamps on the first DELTA — the first actual token. Earlier events
	// carry none (response.created arrives immediately; output_item events
	// only frame content). A terminal event stamps as a fallback so a stream
	// with no deltas still reports something.
	stampTTFT := func() {
		if ttft < 0 {
			ttft = time.Since(start).Milliseconds()
		}
	}
	for event, err := range model.StreamResponse(c.Request.Context(), mreq) {
		if err != nil {
			writeEvent("error", gin.H{"message": err.Error()})
			return
		}
		if event == nil {
			continue
		}
		switch event.Type {
		case "response.output_text.delta":
			stampTTFT()
			if !writeEvent("delta", gin.H{"text": event.AsResponseOutputTextDelta().Delta}) {
				return
			}
		case "response.reasoning_summary_text.delta":
			stampTTFT()
			if !writeEvent("reasoning", gin.H{"text": event.AsResponseReasoningSummaryTextDelta().Delta}) {
				return
			}
		case "response.reasoning_text.delta":
			// Raw reasoning deltas — the Anthropic adapter's thinking stream;
			// OpenAI sends summaries through the case above.
			stampTTFT()
			if !writeEvent("reasoning", gin.H{"text": event.AsResponseReasoningTextDelta().Delta}) {
				return
			}
		case "response.output_item.done":
			acc = append(acc, event.AsResponseOutputItemDone().Item)
		case "response.completed":
			stampTTFT()
			terminal = true
			completed := event.AsResponseCompleted()
			final = completed.Response.Output
			usage = playgroundUsage(&completed.Response)
		case "response.incomplete":
			stampTTFT()
			terminal = true
			inc := event.AsResponseIncomplete()
			final = inc.Response.Output
			usage = playgroundUsage(&inc.Response)
		}
	}
	// The item.done fallback covers a TERMINAL response with an empty Output
	// array — never a stream that broke off before its terminal event, which
	// must report as the failure it is (mirrors the SDK's streaming path).
	if !terminal {
		writeEvent("error", gin.H{"message": "model stream ended without a completed response"})
		return
	}
	if len(final) == 0 {
		final = acc
	}
	out, err := json.Marshal(final)
	if err != nil {
		writeEvent("error", gin.H{"message": err.Error()})
		return
	}
	writeEvent("done", playgroundResp{
		Output:     out,
		Usage:      usage,
		DurationMS: time.Since(start).Milliseconds(),
		TTFTMS:     ttft,
	})
}

// AgentTools returns the agent's CURRENT tool surface as schema-only
// definitions — what BuildFullAgent hands the model right now: bridge
// built-ins, connected MCP servers' tools, the skills reader. No sandbox is
// selected here, so sandbox tools reach a replay only via the traced request.
// Backs the Replay dialog's tool picker, which offers these beyond the traced
// set for what-if replays.
//
//	@Summary		Agent tool surface
//	@Description	Schema-only definitions (name, description, parameters) of every tool the agent would carry right now, excluding sandbox tools (no sandbox is selected). Tools are never executed from here.
//	@Tags			agents
//	@Produce		json
//	@Param			id	path		string	true	"Agent config ID"
//	@Success		200	{array}		playgroundTool
//	@Failure		400	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents/{id}/tools [get]
func (h *PlaygroundHandler) AgentTools(c *gin.Context) {
	built, err := bridge.BuildFullAgent(c.Request.Context(), h.deps, c.Param("id"), "")
	if err != nil {
		badRequest(c, "building agent: "+err.Error())
		return
	}
	// This endpoint reports the FULL surface. A plan-mode build starts in the
	// planning phase, which filters MCP listings; unlock first — this build
	// serves no run, so the phase flag guards nothing here. Per-tool enabled
	// hooks are per-run dynamic and deliberately not evaluated.
	if built.PlanPhase != nil {
		_ = built.PlanPhase.Unlock() // a fresh build has no hook armed; cannot fail
	}
	describe := func(t *agents.Tool) playgroundTool {
		return playgroundTool{Name: t.Name, Description: t.Description, Parameters: t.ParamsJSONSchema}
	}
	out := make([]playgroundTool, 0, len(built.Agent.Tools))
	for _, t := range built.Agent.Tools {
		out = append(out, describe(t))
	}
	// Connected MCP servers are part of the surface too. A server whose
	// listing fails is skipped rather than failing the endpoint: this feeds a
	// picker, and one broken server should not blank the whole list.
	for _, srv := range built.Agent.MCPServers {
		tools, lerr := srv.ListTools(c.Request.Context(), nil, built.Agent)
		if lerr != nil {
			continue
		}
		for _, t := range tools {
			out = append(out, describe(t))
		}
	}
	c.JSON(http.StatusOK, out)
}

// playgroundUsage extracts token usage from a streamed final response,
// mirroring the SDK's internal streaming usage assembly.
func playgroundUsage(resp *responses.Response) *agents.Usage {
	if !resp.JSON.Usage.Valid() {
		return nil
	}
	u := resp.Usage
	return &agents.Usage{
		Requests:     1,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
}

func errString(err error) string {
	if err == nil {
		return "model not found"
	}
	return err.Error()
}
