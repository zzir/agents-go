package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AgentSpec is an agent config's JSON-encoded fields decoded once into typed
// values. Save-time validation (handler.validateAgentConfig) and the build
// (buildAgentFromConfig) both go through DecodeAgentSpec, so each field's
// structural contract is defined in one place; a decode error is a bad config
// (400 at save, a loud build failure at run). External resolution (guardrail
// names, MCP server ids, sandbox) is NOT here — it needs live stores.
type AgentSpec struct {
	// ModelSettings is nil when unset. extra_body (which ModelSettings does not
	// itself model) is merged in from the same JSON object.
	ModelSettings *agents.ModelSettings
	// OutputType is nil when no structured-output schema is configured.
	OutputType agents.OutputSchema
	// ApproveTools is the HITL approval tool-name list (nil when unset/empty).
	ApproveTools []string
	// Tools is the selected MCP server id list (nil when unset).
	Tools []string
	// Skills is the per-agent skill selection (stored ids); SkillsSet tells an
	// unset selection (every stored skill) from an explicit empty one.
	Skills    []string
	SkillsSet bool
	// Handoffs is the handoff target agent-id list (nil when unset).
	Handoffs []string
	// RetryPolicy is decoded unconditionally (the zero value is a valid policy)
	// and applied only when RetryEnabled.
	RetryPolicy agents.RetryPolicy
	// FallbackModels is the decoded fallback provider chain (nil when unset).
	FallbackModels []fallbackEntry
	// ErrorHandlers is the declarative run-error recovery config (nil when
	// unset): per-error-kind static fallback outputs.
	ErrorHandlers *ErrorHandlersSpec
}

// ErrorHandlersSpec is the decoded error_handlers config field: for each run
// error kind, a static fallback that turns the failure into a normal
// completion. Only the top-level agent's spec applies — like max_turns it is
// forwarded to run-level options, so handoff targets share the run's handlers.
type ErrorHandlersSpec struct {
	MaxTurns           *ErrorHandlerEntry `json:"max_turns,omitempty"`
	ModelRefusal       *ErrorHandlerEntry `json:"model_refusal,omitempty"`
	InvalidFinalOutput *ErrorHandlerEntry `json:"invalid_final_output,omitempty"`
}

// ErrorHandlerEntry is one kind's static fallback: the final output the run
// completes with (a JSON value — a string for plain-text agents, an object
// matching the output schema for structured ones) and whether to keep the
// synthesized assistant message out of the conversation history.
type ErrorHandlerEntry struct {
	FinalOutput        json.RawMessage `json:"final_output"`
	ExcludeFromHistory bool            `json:"exclude_from_history,omitempty"`
}

// decodeErrorHandlers parses error_handlers: unknown keys rejected, every entry
// needs a final_output, and a plain-text agent's must be a JSON string.
func decodeErrorHandlers(raw string, outputType agents.OutputSchema) (*ErrorHandlersSpec, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec ErrorHandlersSpec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("error_handlers is invalid: %w", err)
	}
	for _, kind := range []struct {
		name  string
		entry *ErrorHandlerEntry
	}{
		{"max_turns", spec.MaxTurns},
		{"model_refusal", spec.ModelRefusal},
		{"invalid_final_output", spec.InvalidFinalOutput},
	} {
		if kind.entry == nil {
			continue
		}
		if len(kind.entry.FinalOutput) == 0 {
			return nil, fmt.Errorf("error_handlers.%s: final_output is required", kind.name)
		}
		if !json.Valid(kind.entry.FinalOutput) {
			return nil, fmt.Errorf("error_handlers.%s: final_output is not valid JSON", kind.name)
		}
		if outputType == nil {
			var s string
			if err := json.Unmarshal(kind.entry.FinalOutput, &s); err != nil {
				return nil, fmt.Errorf("error_handlers.%s: final_output must be a JSON string for a plain-text agent (configure output_schema for structured fallbacks)", kind.name)
			}
		}
	}
	return &spec, nil
}

// BuildErrorHandlers converts the declarative spec into the SDK's run-level
// handlers: each configured kind returns its static fallback. A nil spec (or
// kind) leaves that error fatal. The fallback of an agent with an output
// schema is validated against it by the SDK when the handler fires.
func (s *ErrorHandlersSpec) BuildErrorHandlers() agents.RunErrorHandlers {
	if s == nil {
		return agents.RunErrorHandlers{}
	}
	return agents.RunErrorHandlers{
		MaxTurns:           s.MaxTurns.staticHandler(),
		ModelRefusal:       s.ModelRefusal.staticHandler(),
		InvalidFinalOutput: s.InvalidFinalOutput.staticHandler(),
	}
}

// staticHandler returns a RunErrorHandler that always recovers with the
// entry's fallback, or nil when the entry is not configured.
func (e *ErrorHandlerEntry) staticHandler() agents.RunErrorHandler {
	if e == nil {
		return nil
	}
	// Decode once: a plain-text agent's final output must be the string value
	// itself, and RunErrorData consumers expect plain Go values.
	var v any
	if err := json.Unmarshal(e.FinalOutput, &v); err != nil {
		return nil // unreachable: decodeErrorHandlers validated the JSON
	}
	exclude := e.ExcludeFromHistory
	return func(_ context.Context, _ agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
		return &agents.RunErrorHandlerResult{FinalOutput: v, ExcludeFromHistory: exclude}, nil
	}
}

// DecodeAgentSpec decodes every JSON-encoded field of an agent config into typed
// values exactly once, returning the first structural error (unprefixed, so the
// caller can wrap it with "agent %q:" or surface it verbatim to an API client).
// It performs no I/O and resolves no external references.
func DecodeAgentSpec(ac *store.AgentConfig) (*AgentSpec, error) {
	spec := &AgentSpec{}

	// Enum fields are refused at save, not coerced at run: an unknown value
	// would parse as "error" while the UI showed something else.
	switch ac.Behavior.ToolNotFoundBehavior {
	case "", "return_to_model", "return_error_to_model", "error":
	default:
		return nil, fmt.Errorf("tool_not_found_behavior %q: use return_to_model, error, or leave it unset", ac.Behavior.ToolNotFoundBehavior)
	}
	switch ac.Behavior.ReasoningItemIDPolicy {
	case "", "preserve", "omit":
	default:
		return nil, fmt.Errorf("reasoning_item_id_policy %q: use preserve, omit, or leave it unset", ac.Behavior.ReasoningItemIDPolicy)
	}

	if ac.ModelSettings != "" {
		var ms agents.ModelSettings
		// Malformed or wrong-typed model_settings is rejected rather than
		// silently running on defaults.
		if err := json.Unmarshal([]byte(ac.ModelSettings), &ms); err != nil {
			return nil, fmt.Errorf("model_settings is invalid: %w", err)
		}
		// extra_body is not a ModelSettings field; carry it over from the same
		// raw object so a configured extra_body survives the decode.
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(ac.ModelSettings), &raw) == nil {
			if eb, ok := raw["extra_body"]; ok {
				var extraBody map[string]any
				if json.Unmarshal(eb, &extraBody) == nil && len(extraBody) > 0 {
					ms.ExtraBody = extraBody
				}
			}
		}
		spec.ModelSettings = &ms
	}

	if ac.Guardrails.OutputSchema != "" {
		// BuildOutputSchema's error already names output_schema.
		os, err := BuildOutputSchema(ac.Guardrails.OutputSchema)
		if err != nil {
			return nil, err
		}
		spec.OutputType = os
	}

	if err := decodeStringList(ac.Approval.ApproveTools, "approve_tools", &spec.ApproveTools); err != nil {
		return nil, err
	}
	if err := decodeStringList(ac.ToolsJSON, "tools", &spec.Tools); err != nil {
		return nil, err
	}
	if err := decodeStringList(ac.HandoffsJSON, "handoffs", &spec.Handoffs); err != nil {
		return nil, err
	}
	if ac.SkillsJSON != "" {
		// Fail rather than fall open: a malformed skills selection must not
		// leave the full skill set attached, widening the capability surface.
		if err := json.Unmarshal([]byte(ac.SkillsJSON), &spec.Skills); err != nil {
			return nil, fmt.Errorf("skills selection is invalid: %w", err)
		}
		spec.SkillsSet = true
	}

	if ac.Resilience.RetryPolicy != "" {
		if err := json.Unmarshal([]byte(ac.Resilience.RetryPolicy), &spec.RetryPolicy); err != nil {
			return nil, fmt.Errorf("retry_policy is invalid: %w", err)
		}
	}
	if ac.Resilience.FallbackModels != "" {
		// Unknown keys are rejected: a misspelled selector would silently run
		// the entry on the default backend.
		dec := json.NewDecoder(strings.NewReader(ac.Resilience.FallbackModels))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec.FallbackModels); err != nil {
			return nil, fmt.Errorf("fallback_models is invalid: %w", err)
		}
		// Decode stops after the first JSON value (unlike Unmarshal); trailing
		// content is a malformed config, not something to silently drop.
		if dec.More() {
			return nil, fmt.Errorf("fallback_models is invalid: trailing data after the JSON array")
		}
		for i, e := range spec.FallbackModels {
			if err := providers.ValidateType(e.Provider); err != nil {
				return nil, fmt.Errorf("fallback_models[%d].provider_type: %w", i, err)
			}
		}
	}

	if ac.ErrorHandlers != "" {
		// Depends on spec.OutputType (decoded above): a plain-text agent's
		// fallback must be a JSON string.
		eh, err := decodeErrorHandlers(ac.ErrorHandlers, spec.OutputType)
		if err != nil {
			return nil, err
		}
		spec.ErrorHandlers = eh
	}

	return spec, nil
}

// decodeStringList decodes a JSON string-array field into dst, leaving it nil
// when the field is unset. label names the field in the error message.
func decodeStringList(raw, label string, dst *[]string) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("%s must be a JSON array of strings: %w", label, err)
	}
	return nil
}
