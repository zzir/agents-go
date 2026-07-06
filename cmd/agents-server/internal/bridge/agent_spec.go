package bridge

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AgentSpec is an agent config's JSON-encoded fields decoded once into typed
// values. Both save-time validation (handler.validateAgentConfig) and the build
// (buildAgentFromConfig) go through DecodeAgentSpec, so a field's structural
// contract — is model_settings a valid ModelSettings, is approve_tools a JSON
// string array — is defined in exactly one place. A decode error is a bad
// config: the handler maps it to 400, the build fails loudly rather than
// silently running on defaults the operator never chose.
//
// External resolution (guardrail names, MCP server ids, sandbox) is deliberately
// NOT here — it needs live stores and stays at its call site.
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
	// Skills is the per-agent skill selection. SkillsSet distinguishes an unset
	// selection (get every installed skill) from an explicit empty one.
	Skills    []string
	SkillsSet bool
	// Handoffs is the handoff target agent-id list (nil when unset).
	Handoffs []string
	// RetryPolicy is decoded to validate it and to feed NewRetryProvider; the
	// zero value is a valid (default) policy, so decode is unconditional and the
	// build applies it only when RetryEnabled.
	RetryPolicy agents.RetryPolicy
	// FallbackModels is the decoded fallback provider chain (nil when unset).
	FallbackModels []fallbackEntry
}

// DecodeAgentSpec decodes every JSON-encoded field of an agent config into typed
// values exactly once, returning the first structural error (unprefixed, so the
// caller can wrap it with "agent %q:" or surface it verbatim to an API client).
// It performs no I/O and resolves no external references.
func DecodeAgentSpec(ac *store.AgentConfig) (*AgentSpec, error) {
	spec := &AgentSpec{}

	if ac.ModelSettings != "" {
		var ms agents.ModelSettings
		// A malformed or wrong-typed model_settings (e.g. temperature: "hot")
		// is rejected rather than silently running on defaults the operator
		// would think took effect.
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

	if ac.OutputSchema != "" {
		// BuildOutputSchema's error already names output_schema.
		os, err := BuildOutputSchema(ac.OutputSchema)
		if err != nil {
			return nil, err
		}
		spec.OutputType = os
	}

	if err := decodeStringList(ac.ApproveTools, "approve_tools", &spec.ApproveTools); err != nil {
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

	if ac.RetryPolicy != "" {
		if err := json.Unmarshal([]byte(ac.RetryPolicy), &spec.RetryPolicy); err != nil {
			return nil, fmt.Errorf("retry_policy is invalid: %w", err)
		}
	}
	if ac.FallbackModels != "" {
		if err := json.Unmarshal([]byte(ac.FallbackModels), &spec.FallbackModels); err != nil {
			return nil, fmt.Errorf("fallback_models is not valid JSON: %w", err)
		}
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
