package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents"
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
	// calls. They are never executed: a single GetResponse runs no tool loop.
	Tools []playgroundTool `json:"tools,omitempty"`
}

type playgroundTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type playgroundResp struct {
	Output     json.RawMessage `json:"output"`
	Usage      *agents.Usage   `json:"usage,omitempty"`
	DurationMS int64           `json:"duration_ms"`
}

// Generate performs a single non-streaming model call with the given
// instructions and input items, using the agent's provider configuration.
//
//	@Summary		Playground generate
//	@Description	One-off model call for replaying a traced generation with edited inputs. Touches no session, records no run; tools are schema-only and never executed.
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
	if modelName == "" {
		modelName = built.Agent.Model
	}
	model, err := built.Provider.GetModel(modelName)
	if err != nil || model == nil {
		badRequest(c, "resolving model: "+errString(err))
		return
	}

	items := make([]agents.TResponseInputItem, 0, len(req.InputItems))
	for i, raw := range req.InputItems {
		item, err := agents.UnmarshalInputItem(store.NormalizeItemJSON(raw))
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
	var tools []agents.Tool
	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		tools = append(tools, &agents.FunctionTool{
			Name:             t.Name,
			Description:      t.Description,
			ParamsJSONSchema: t.Parameters,
			OnInvoke: func(context.Context, *agents.ToolContext, string) (any, error) {
				return nil, errors.New("playground tools are not executable")
			},
		})
	}

	start := time.Now()
	resp, err := model.GetResponse(c.Request.Context(), agents.ModelRequest{
		SystemInstructions: req.SystemInstructions,
		Input:              items,
		Settings:           settings,
		Tools:              tools,
		Tracing:            agents.ModelTracingDisabled,
	})
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
	})
}

func errString(err error) string {
	if err == nil {
		return "model not found"
	}
	return err.Error()
}
