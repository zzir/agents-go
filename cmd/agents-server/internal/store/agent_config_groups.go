package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// The AgentConfig scalar settings are grouped into a handful of JSON category
// columns rather than one column per knob, so adding a setting needs no schema
// change (the table only holds category columns). Each group is stored as a JSON
// text column via the Value/Scan pair below and serializes to a nested object in
// the REST API.

// BehaviorGroup holds the run-behavior knobs.
//
// Booleans are stated POSITIVELY (decisions §5.43): the field names the
// capability, true turns it on. A knob whose default is ON uses *bool — nil
// (the key absent, every row from before the knob existed) means the
// default, so shipping the knob never flips existing agents.
type BehaviorGroup struct {
	MaxTurns           int    `json:"max_turns,omitempty"`
	HandoffDescription string `json:"handoff_description,omitempty"`
	// ToolChoiceReset resets a pinned tool_choice after a tool runs (the
	// SDK's default loop-guard). nil/true = on; false keeps tool_choice
	// as-is across turns.
	ToolChoiceReset *bool `json:"tool_choice_reset,omitempty"`
	// StopAtTools is a comma-separated list of tool names; the run ends after a
	// turn that called any of them, instead of feeding the results back to the
	// model. Empty means the run continues until the model stops on its own.
	StopAtTools          string `json:"stop_at_tools,omitempty"`
	HandoffInputFilter   string `json:"handoff_input_filter,omitempty"`
	MaxToolConcurrency   int    `json:"max_tool_concurrency,omitempty"`
	ToolNotFoundBehavior string `json:"tool_not_found_behavior,omitempty"`
	// ReasoningItemIDPolicy is "" / "preserve" (keep reasoning-item ids across
	// turns) or "omit" (strip them).
	ReasoningItemIDPolicy string `json:"reasoning_item_id_policy,omitempty"`
	// WorkflowAuthoring gives the agent's chat runs get_workflow / save_workflow
	// (workbench invariant 39). Off by default: the save schema costs every request.
	WorkflowAuthoring bool `json:"workflow_authoring,omitempty"`
	// Subagents grants the agent's chat runs spawn_task / task_status /
	// task_stop / task_retry. nil/true = on; a chat-only agent that never
	// delegates sets false to reclaim the task schema from every request.
	Subagents *bool `json:"subagents,omitempty"`
	// Vision admits image attachments on this agent's runs. Off by default:
	// the gate is what turns "model returned 400" into a config error a
	// person can act on, so it must be an explicit claim that the model
	// accepts image input.
	Vision bool `json:"vision,omitempty"`
}

// SubagentsOn reports whether the agent's chat runs get the task tools;
// nil (unset) is on.
func (g BehaviorGroup) SubagentsOn() bool { return g.Subagents == nil || *g.Subagents }

// ToolChoiceResetOn reports whether tool_choice resets after a tool runs;
// nil (unset) is on.
func (g BehaviorGroup) ToolChoiceResetOn() bool {
	return g.ToolChoiceReset == nil || *g.ToolChoiceReset
}

// ResilienceGroup holds model retry/fallback settings.
type ResilienceGroup struct {
	RetryEnabled   bool   `json:"retry_enabled,omitempty"`
	RetryPolicy    string `json:"retry_policy,omitempty"`
	FallbackModels string `json:"fallback_models,omitempty"`
}

// GuardrailGroup holds guardrail names and the output schema.
type GuardrailGroup struct {
	// Guardrails is a JSON array of guardrail names. One list, not one per
	// stage: a guardrail carries the stages it inspects, so naming it twice
	// would be naming the same value twice.
	Guardrails   string `json:"guardrails,omitempty"`
	OutputSchema string `json:"output_schema,omitempty"`
}

// SessionGroup holds session/prompt settings.
//
// use_previous_response_id is deliberately ABSENT: agents-server always
// persists history in a server-side session, which the SDK refuses to combine
// with previous-response chaining. The field spent its life as a dead switch
// (stored, surfaced, then rejected at build); a legacy row whose session JSON
// still carries the key simply decodes past it.
type SessionGroup struct {
	PromptID      string `json:"prompt_id,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`
	// HistoryLimit caps how many recent session items each turn loads (0 = all).
	HistoryLimit int `json:"history_limit,omitempty"`
}

// ApprovalGroup holds the HITL approval selection (JSON: ["*"] or names).
type ApprovalGroup struct {
	ApproveTools string `json:"approve_tools,omitempty"`
}

// CompactionGroup holds server-side session-compaction settings.
type CompactionGroup struct {
	Enabled bool `json:"compaction_enabled,omitempty"`
	// Threshold is in TOKENS. The key is compaction_threshold_tokens — a NEW
	// name because the earlier compaction_threshold counted ENTRIES, and
	// reinterpreting a stored 20 as tokens would compact on every turn. Legacy
	// rows decode past the old key and fall back to the default.
	Threshold int    `json:"compaction_threshold_tokens,omitempty"`
	Window    int    `json:"compaction_window,omitempty"`
	Model     string `json:"compaction_model,omitempty"`
	Prompt    string `json:"compaction_prompt,omitempty"`
}

// jsonGroupValue / jsonGroupScan back the driver.Valuer / sql.Scanner
// implementations below, so each group struct round-trips as a JSON text column.
func jsonGroupValue(v any) (driver.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func jsonGroupScan(dst, src any) error {
	if src == nil {
		return nil
	}
	var b []byte
	switch s := src.(type) {
	case []byte:
		b = s
	case string:
		b = []byte(s)
	default:
		return fmt.Errorf("agent config group: cannot scan %T", src)
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, dst)
}

// Value implements driver.Valuer.
func (g BehaviorGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *BehaviorGroup) Scan(src any) error { return jsonGroupScan(g, src) }

// Value implements driver.Valuer.
func (g ResilienceGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *ResilienceGroup) Scan(src any) error { return jsonGroupScan(g, src) }

// Value implements driver.Valuer.
func (g GuardrailGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *GuardrailGroup) Scan(src any) error { return jsonGroupScan(g, src) }

// Value implements driver.Valuer.
func (g SessionGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *SessionGroup) Scan(src any) error { return jsonGroupScan(g, src) }

// Value implements driver.Valuer.
func (g ApprovalGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *ApprovalGroup) Scan(src any) error { return jsonGroupScan(g, src) }

// Value implements driver.Valuer.
func (g CompactionGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *CompactionGroup) Scan(src any) error { return jsonGroupScan(g, src) }
