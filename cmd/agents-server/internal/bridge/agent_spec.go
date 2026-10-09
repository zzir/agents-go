package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AgentSpec is an agent config's JSON-encoded fields decoded once into typed
// values, through DecodeAgentSpec at save time and at build time alike.
// External resolution (guardrail names, MCP server ids, sandbox) is NOT here.
type AgentSpec struct {
	// ModelSettings is nil when unset. extra_body (which ModelSettings does not
	// itself model) is merged in from the same JSON object.
	ModelSettings *agents.ModelSettings
	// OutputType is nil when no structured-output schema is configured.
	OutputType agents.OutputSchema
	// Approval is the HITL selection: the mode and the tool-name list (nil when
	// unset/empty).
	Approval store.ApprovalGroup
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
	// FallbackModels is the fallback provider chain (nil when unset).
	FallbackModels []store.FallbackModel
	// ErrorHandlers is the declarative run-error recovery config (nil when
	// unset): per-error-kind static fallback outputs.
	ErrorHandlers *ErrorHandlersSpec
}

// ErrorHandlersSpec is the decoded error_handlers config field: per run error
// kind, a static fallback that turns the failure into a normal completion.
// Only the top-level agent's applies (run-level options, like max_turns).
type ErrorHandlersSpec struct {
	MaxTurns           *ErrorHandlerEntry `json:"max_turns,omitempty"`
	ModelRefusal       *ErrorHandlerEntry `json:"model_refusal,omitempty"`
	InvalidFinalOutput *ErrorHandlerEntry `json:"invalid_final_output,omitempty"`
}

// ErrorHandlerEntry is one kind's static fallback: the final output the run
// completes with (a JSON string for a plain-text agent, else an object of the
// output schema) and whether to keep the synthesized message out of the history.
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

// BuildErrorHandlers converts the spec into the SDK's run-level handlers, each
// configured kind returning its static fallback; a nil spec (or kind) leaves
// that error fatal. The SDK validates a fallback against the output schema when
// it fires.
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
	// Decoded once: RunErrorData consumers expect plain Go values.
	var v any
	if err := json.Unmarshal(e.FinalOutput, &v); err != nil {
		return nil // unreachable: decodeErrorHandlers validated the JSON
	}
	exclude := e.ExcludeFromHistory
	return func(_ context.Context, _ agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
		return &agents.RunErrorHandlerResult{FinalOutput: v, ExcludeFromHistory: exclude}, nil
	}
}

// DecodeAgentSpec decodes every JSON-encoded field of an agent config into
// typed values exactly once, returning the first structural error unprefixed
// (the caller adds "agent %q:"). No I/O, no external references.
func DecodeAgentSpec(ac *store.AgentConfig) (*AgentSpec, error) {
	spec := &AgentSpec{}

	// Enum fields are refused at save, not coerced at run.
	switch ac.Compaction.Mode {
	case "", store.CompactionModeSummary, store.CompactionModeReset, store.CompactionModeHybrid:
	default:
		return nil, fmt.Errorf("compaction_mode %q: use summary, reset, hybrid, or leave it unset", ac.Compaction.Mode)
	}
	if ac.Compaction.ResetMode() && !ac.Compaction.Enabled {
		return nil, fmt.Errorf("compaction_mode %q needs compaction_enabled", ac.Compaction.Mode)
	}
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
	switch ac.Behavior.ThinkingMode {
	case "", providers.ThinkingModeBudget:
	default:
		return nil, fmt.Errorf("thinking_mode %q: use budget, or leave it unset", ac.Behavior.ThinkingMode)
	}

	if ac.ModelSettings != "" {
		var ms agents.ModelSettings
		// Malformed or wrong-typed model_settings is rejected.
		if err := json.Unmarshal([]byte(ac.ModelSettings), &ms); err != nil {
			return nil, fmt.Errorf("model_settings is invalid: %w", err)
		}
		// extra_body is not a ModelSettings field; carried over from the same
		// raw object.
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

	switch ac.Approval.Mode {
	case "", store.ApprovalModeNever, store.ApprovalModeOnChange, store.ApprovalModeAlways:
	default:
		return nil, fmt.Errorf("approval mode %q: use never, on_change, always, or leave it unset", ac.Approval.Mode)
	}
	// "*" would route exec_command around its per-command gate (invariant 90).
	if ac.Approval.Asks() && slices.Contains(ac.Approval.ApproveTools, "*") {
		return nil, fmt.Errorf("approve_tools %q: with approval mode %s, name the tools to add or leave the list empty", "*", ac.Approval.Mode)
	}
	spec.Approval = ac.Approval
	spec.Tools = ac.Tools
	spec.Handoffs = ac.Handoffs
	// A nil selection is "every skill"; an explicit [] is none.
	spec.Skills, spec.SkillsSet = ac.Skills, ac.Skills != nil

	if ac.Resilience.RetryPolicy != "" {
		if err := json.Unmarshal([]byte(ac.Resilience.RetryPolicy), &spec.RetryPolicy); err != nil {
			return nil, fmt.Errorf("retry_policy is invalid: %w", err)
		}
	}
	// An entry without provider_id names an endpoint, resolved when the run
	// builds (provider_resolve.go).
	spec.FallbackModels = ac.Resilience.FallbackModels

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
