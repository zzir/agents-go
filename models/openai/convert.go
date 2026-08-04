// Package openai implements the agents Model interface against the OpenAI
// Responses API, using the official openai-go SDK. The older Chat Completions
// API is intentionally not supported.
package openai

import (
	"fmt"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zzir/agents-go/agents"
)

// convertTools translates the SDK's tools and handoffs into Responses API tool
// params. Tools are provider-agnostic: every Tool is a FunctionTool the SDK
// executes locally (handoffs are modeled as function tools too), so the
// conversion only emits function-tool params. The SDK has no provider-hosted
// tool types.
func convertTools(tools []agents.Tool, handoffs []agents.Handoff) ([]responses.ToolUnionParam, error) {
	out := make([]responses.ToolUnionParam, 0, len(tools)+len(handoffs))
	for _, t := range tools {
		// Through ToolAs, not a bare assertion: a decorated tool (approval,
		// timeout, guardrails) still has to reach the model with the same
		// name and schema as the tool underneath it.
		d, ok := agents.ToolAs[agents.DescribableTool](t)
		if !ok {
			return nil, fmt.Errorf("openai: unsupported tool type %T (only function tools are supported)", t)
		}
		out = append(out, functionToolParam(t.ToolName(), d.ToolDescription(), d.ToolParamsSchema(), d.ToolStrict()))
	}
	for _, h := range handoffs {
		out = append(out, functionToolParam(h.ToolName, h.ToolDescription, h.InputJSONSchema, !h.NonStrictSchema))
	}
	return out, nil
}

func functionToolParam(name, description string, schema map[string]any, strict bool) responses.ToolUnionParam {
	if schema == nil {
		// "parameters" is required by the API; a hand-built tool without a
		// schema would otherwise serialize with the field omitted entirely.
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
		if strict {
			schema["additionalProperties"] = false
			schema["required"] = []any{}
		}
	}
	fn := &responses.FunctionToolParam{
		Name:       name,
		Parameters: schema,
		Strict:     oai.Bool(strict),
	}
	if description != "" {
		fn.Description = oai.String(description)
	}
	return responses.ToolUnionParam{OfFunction: fn}
}

// convertToolChoice maps an SDK tool-choice string to the Responses API union.
// An empty choice leaves the field omitted (provider default); any value other
// than auto/required/none is treated as a specific function tool name.
func convertToolChoice(choice agents.ToolChoice) (responses.ResponseNewParamsToolChoiceUnion, bool) {
	switch choice {
	case "":
		return responses.ResponseNewParamsToolChoiceUnion{}, false
	case agents.ToolChoiceAuto:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: oai.Opt(responses.ToolChoiceOptionsAuto)}, true
	case agents.ToolChoiceRequired:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: oai.Opt(responses.ToolChoiceOptionsRequired)}, true
	case agents.ToolChoiceNone:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: oai.Opt(responses.ToolChoiceOptionsNone)}, true
	default:
		// A specific function tool name.
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: string(choice)},
		}, true
	}
}

// responseFormat builds the Responses API text/format payload for the agent's
// output schema. It mirrors Converter.get_response_format. The bool reports
// whether a structured format was set.
func responseFormat(schema agents.OutputSchema) (responses.ResponseTextConfigParam, bool) {
	if schema == nil || schema.IsPlainText() {
		return responses.ResponseTextConfigParam{}, false
	}
	return responses.ResponseTextConfigParam{
		Format: responses.ResponseFormatTextConfigUnionParam{
			OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
				Name:   schema.Name(),
				Schema: schema.JSONSchema(),
				Strict: oai.Bool(schema.IsStrictJSONSchema()),
			},
		},
	}, true
}

// applySettings overlays the model settings onto the request params, mirroring
// the field mapping in _build_response_create_kwargs.
func applySettings(params *responses.ResponseNewParams, s *agents.ModelSettings, hasTools bool) {
	if s == nil {
		return
	}
	if s.Temperature != nil {
		params.Temperature = oai.Float(*s.Temperature)
	}
	if s.TopP != nil {
		params.TopP = oai.Float(*s.TopP)
	}
	if s.MaxTokens != nil {
		params.MaxOutputTokens = oai.Int(*s.MaxTokens)
	}
	if s.TopLogprobs != nil {
		params.TopLogprobs = oai.Int(*s.TopLogprobs)
	}
	if s.Store != nil {
		params.Store = oai.Bool(*s.Store)
	}
	if s.Truncation != "" {
		params.Truncation = responses.ResponseNewParamsTruncation(s.Truncation)
	}
	if s.PromptCacheRetention != "" {
		params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention(s.PromptCacheRetention)
	}
	if s.PromptCacheKey != "" {
		params.PromptCacheKey = oai.String(s.PromptCacheKey)
	}
	if s.PromptCacheOptions != nil {
		params.PromptCacheOptions = responses.ResponseNewParamsPromptCacheOptions{
			Mode: string(s.PromptCacheOptions.Mode),
			Ttl:  s.PromptCacheOptions.TTL,
		}
	}
	if len(s.ContextManagement) > 0 {
		entries := make([]responses.ResponseNewParamsContextManagement, len(s.ContextManagement))
		for i, cm := range s.ContextManagement {
			entries[i] = responses.ResponseNewParamsContextManagement{Type: string(cm.Type)}
			if cm.CompactThreshold != nil {
				entries[i].CompactThreshold = oai.Int(*cm.CompactThreshold)
			}
		}
		params.ContextManagement = entries
	}
	// parallel_tool_calls only applies when tools are present, matching the
	// Python SDK's gating.
	if s.ParallelToolCalls != nil {
		if *s.ParallelToolCalls && hasTools {
			params.ParallelToolCalls = oai.Bool(true)
		} else if !*s.ParallelToolCalls {
			params.ParallelToolCalls = oai.Bool(false)
		}
	}
	if len(s.Metadata) > 0 {
		params.Metadata = shared.Metadata(s.Metadata)
	}
	if s.ServiceTier != "" {
		params.ServiceTier = responses.ResponseNewParamsServiceTier(s.ServiceTier)
	}
	if s.Reasoning != nil {
		params.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(s.Reasoning.Effort),
			Summary: shared.ReasoningSummary(s.Reasoning.Summary),
		}
	}
}

// convertPrompt translates an agents.Prompt into the Responses API prompt
// parameter. Variable values must be strings (text substitutions); a non-string
// value (e.g. an intended image/file content variable, which is not modeled
// here) is rejected with a *UserError rather than silently stringified.
func convertPrompt(p *agents.Prompt) (responses.ResponsePromptParam, error) {
	out := responses.ResponsePromptParam{ID: p.ID}
	if p.Version != "" {
		out.Version = oai.String(p.Version)
	}
	if len(p.Variables) > 0 {
		out.Variables = make(map[string]responses.ResponsePromptVariableUnionParam, len(p.Variables))
		for k, v := range p.Variables {
			s, ok := v.(string)
			if !ok {
				return responses.ResponsePromptParam{}, agents.NewUserError(
					"prompt variable %q has unsupported type %T: only string values are supported", k, v)
			}
			out.Variables[k] = responses.ResponsePromptVariableUnionParam{OfString: oai.String(s)}
		}
	}
	return out, nil
}
