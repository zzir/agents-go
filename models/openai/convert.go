// Package openai implements the agents Model interface against the OpenAI
// Responses and Chat Completions APIs, using the official openai-go SDK.
package openai

import (
	"fmt"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/zzir/agents-go/agents"
)

// convertTools translates the SDK's tools and handoffs into Responses API tool
// params. It mirrors Converter.convert_tools in the Python SDK (function tools
// and handoffs only for now; hosted tools land in a later phase).
func convertTools(tools []agents.Tool, handoffs []agents.Handoff) ([]responses.ToolUnionParam, error) {
	out := make([]responses.ToolUnionParam, 0, len(tools)+len(handoffs))
	for _, t := range tools {
		ft, ok := t.(*agents.FunctionTool)
		if !ok {
			return nil, fmt.Errorf("openai: unsupported tool type %T (only function tools are supported in this phase)", t)
		}
		out = append(out, functionToolParam(ft.Name, ft.Description, ft.ParamsJSONSchema, ft.Strict))
	}
	for _, h := range handoffs {
		out = append(out, functionToolParam(h.ToolName, h.ToolDescription, h.InputJSONSchema, h.StrictJSONSchema))
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

// hostedToolChoices are tool_choice values that select a hosted tool type on
// the Responses API. Hosted tools are not supported yet, so these must not be
// silently sent as function names.
var hostedToolChoices = map[string]bool{
	"file_search": true, "web_search": true, "web_search_preview": true,
	"computer_use_preview": true, "image_generation": true,
	"code_interpreter": true, "mcp": true,
}

// convertToolChoice maps an SDK tool-choice string to the Responses API union.
// An empty choice leaves the field omitted (provider default).
func convertToolChoice(choice agents.ToolChoice) (responses.ResponseNewParamsToolChoiceUnion, bool, error) {
	switch choice {
	case "":
		return responses.ResponseNewParamsToolChoiceUnion{}, false, nil
	case agents.ToolChoiceAuto:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: oai.Opt(responses.ToolChoiceOptionsAuto)}, true, nil
	case agents.ToolChoiceRequired:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: oai.Opt(responses.ToolChoiceOptionsRequired)}, true, nil
	case agents.ToolChoiceNone:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: oai.Opt(responses.ToolChoiceOptionsNone)}, true, nil
	default:
		if hostedToolChoices[choice] {
			return responses.ResponseNewParamsToolChoiceUnion{}, false,
				fmt.Errorf("openai: tool_choice %q selects a hosted tool, which is not supported yet", choice)
		}
		// A specific function tool name.
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice},
		}, true, nil
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
	if s.Reasoning != nil {
		params.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(s.Reasoning.Effort),
			Summary: shared.ReasoningSummary(s.Reasoning.Summary),
		}
	}
}
