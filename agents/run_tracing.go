package agents

import (
	"slices"

	"github.com/zzir/agents-go/tracing"
)

// setGenerationUsage records one model call's token counts on its generation span;
// rc.Usage holds the run-wide accumulation separately.
func setGenerationUsage(span *tracing.SpanHandle, u *Usage) {
	if u == nil {
		return
	}
	span.Set("input_tokens", u.InputTokens)
	span.Set("output_tokens", u.OutputTokens)
	span.Set("total_tokens", u.TotalTokens)
}

// traceIncludeSensitiveData resolves RunOptions.Observe.IncludeSensitiveData; nil
// means include, and no environment variable overrides it — see spec §2.14.
func (r *runner) traceIncludeSensitiveData() bool {
	if r.opts.Observe.IncludeSensitiveData != nil {
		return *r.opts.Observe.IncludeSensitiveData
	}
	return true
}

// traceTools projects the request's tools into a serializable form — the
// function fields on Tool cannot be marshaled.
func traceTools(tools []*Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		m := map[string]any{"name": t.Name}
		if t.Description != "" {
			m["description"] = t.Description
		}
		if t.ParamsJSONSchema != nil {
			m["parameters"] = t.ParamsJSONSchema
		}
		out = append(out, m)
	}
	return out
}

// traceHandoffs projects the request's handoffs into a serializable form: the names
// plus the description and input schema the model saw.
func traceHandoffs(handoffs []Handoff) []map[string]any {
	out := make([]map[string]any, 0, len(handoffs))
	for _, h := range handoffs {
		m := map[string]any{
			"tool_name":  h.ToolName,
			"agent_name": h.AgentName,
		}
		if h.ToolDescription != "" {
			m["description"] = h.ToolDescription
		}
		if h.InputJSONSchema != nil {
			m["parameters"] = h.InputJSONSchema
		}
		out = append(out, m)
	}
	return out
}

// startGenerationSpan opens one model call's span, recording the request body unless
// sensitive-data tracing is off; slices are cloned, as exporters serialize later.
func (r *runner) startGenerationSpan(agent *Agent, req ModelRequest) *tracing.SpanHandle {
	span := r.trace.StartGenerationSpan(agent.Name, r.agentParentID())
	if span.Span == nil || !r.traceIncludeSensitiveData() {
		return span
	}
	if agent.Model != "" {
		span.Set("model", agent.Model)
	}
	if req.SystemInstructions != "" {
		span.Set("system_instructions", req.SystemInstructions)
	}
	span.Set("input", slices.Clone(req.Input))
	if len(req.Tools) > 0 {
		span.Set("tools", traceTools(req.Tools))
	}
	if len(req.Handoffs) > 0 {
		span.Set("handoffs", traceHandoffs(req.Handoffs))
	}
	if req.Settings != nil {
		span.Set("model_settings", *req.Settings)
	}
	if req.OutputSchema != nil && !req.OutputSchema.IsPlainText() {
		span.Set("output_schema", map[string]any{
			"name":   req.OutputSchema.Name(),
			"schema": req.OutputSchema.JSONSchema(),
			"strict": req.OutputSchema.IsStrictJSONSchema(),
		})
	}
	if req.Prompt != nil {
		span.Set("prompt", *req.Prompt)
	}
	if req.PreviousResponseID != "" {
		span.Set("previous_response_id", req.PreviousResponseID)
	}
	if req.ConversationID != "" {
		span.Set("conversation_id", req.ConversationID)
	}
	return span
}

// finishGenerationSpan records the call's response id, usage and (unless
// sensitive-data tracing is off) output items, then ends the span.
func (r *runner) finishGenerationSpan(span *tracing.SpanHandle, resp *ModelResponse) {
	span.Set("response_id", resp.ResponseID)
	setGenerationUsage(span, resp.Usage)
	if span.Span != nil && r.traceIncludeSensitiveData() {
		span.Set("output", slices.Clone(resp.Output))
	}
	span.Finish()
}
