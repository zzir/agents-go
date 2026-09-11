package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// The AgentConfig scalar settings are grouped into JSON category columns, so
// adding a setting needs no schema change; each group is a nested object in the API.

// BehaviorGroup holds the run-behavior knobs. Booleans are stated POSITIVELY
// (decisions §5.43); a knob whose default is ON uses *bool, nil meaning the default.
type BehaviorGroup struct {
	MaxTurns           int    `json:"max_turns,omitempty"`
	HandoffDescription string `json:"handoff_description,omitempty"`
	// ToolChoiceReset resets a pinned tool_choice after a tool runs; nil/true = on.
	ToolChoiceReset *bool `json:"tool_choice_reset,omitempty"`
	// StopAtTools is a comma-separated list of tool names the run ends after; empty lets the model decide.
	StopAtTools          string `json:"stop_at_tools,omitempty"`
	HandoffInputFilter   string `json:"handoff_input_filter,omitempty"`
	MaxToolConcurrency   int    `json:"max_tool_concurrency,omitempty"`
	ToolNotFoundBehavior string `json:"tool_not_found_behavior,omitempty"`
	// ReasoningItemIDPolicy is "" / "preserve" (keep reasoning-item ids across turns) or "omit".
	ReasoningItemIDPolicy string `json:"reasoning_item_id_policy,omitempty"`
	// WorkflowAuthoring gives the agent's chat runs get_workflow / save_workflow; off by default.
	WorkflowAuthoring bool `json:"workflow_authoring,omitempty"`
	// Subagents grants the agent's chat runs the task tools; nil/true = on.
	Subagents *bool `json:"subagents,omitempty"`
	// Vision admits image attachments on this agent's runs; off by default.
	Vision bool `json:"vision,omitempty"`
	// OverrideSystemPrompt sends this agent's instructions alone, empty included; the global system prompt is not prepended.
	OverrideSystemPrompt bool `json:"override_system_prompt,omitempty"`
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
	// Guardrails is a JSON array of guardrail names; each carries the stages it inspects.
	Guardrails   string `json:"guardrails,omitempty"`
	OutputSchema string `json:"output_schema,omitempty"`
}

// SessionGroup holds session/prompt settings. There is no
// use_previous_response_id: the SDK refuses to combine it with a session.
type SessionGroup struct {
	PromptID      string `json:"prompt_id,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`
	// HistoryLimit caps how many recent session items each turn loads (0 = all).
	HistoryLimit int `json:"history_limit,omitempty"`
}

// ApprovalGroup holds the HITL approval selection.
type ApprovalGroup struct {
	// ApproveTools names the tools that pause for approval before each call; ["*"] means every tool.
	ApproveTools StringList `json:"approve_tools,omitempty"`
}

// StringList is a list field: a JSON array on the API, JSON text in the
// column. nil stores as "" and reads back nil, so absent and [] stay distinct.
type StringList []string

// Value implements driver.Valuer.
func (l StringList) Value() (driver.Value, error) {
	if l == nil {
		return "", nil
	}
	return jsonGroupValue(l)
}

// Scan implements sql.Scanner.
func (l *StringList) Scan(src any) error {
	*l = nil
	return jsonGroupScan(l, src)
}

// CompactionGroup holds server-side session-compaction settings.
type CompactionGroup struct {
	Enabled bool `json:"compaction_enabled,omitempty"`
	// Threshold is in tokens.
	Threshold int    `json:"compaction_threshold_tokens,omitempty"`
	Window    int    `json:"compaction_window,omitempty"`
	Model     string `json:"compaction_model,omitempty"`
	Prompt    string `json:"compaction_prompt,omitempty"`
	// Mode is summary (the default), reset or hybrid.
	Mode string `json:"compaction_mode,omitempty"`
}

// The compaction modes.
const (
	CompactionModeSummary = "summary"
	CompactionModeReset   = "reset"
	CompactionModeHybrid  = "hybrid"
)

// ResetMode reports whether a pass folds by reset rather than summary.
func (g CompactionGroup) ResetMode() bool {
	return g.Mode == CompactionModeReset || g.Mode == CompactionModeHybrid
}

// MemoryGroup holds the model's memory surface.
type MemoryGroup struct {
	// Tools gives the agent's chat runs memory_list / read / search / write /
	// append over the session's memory.
	Tools bool `json:"memory_tools,omitempty"`
	// AgentWrite lets the model propose agent memory as well; each such
	// write waits for the user's approval.
	AgentWrite bool `json:"memory_agent_write,omitempty"`
	// HistoryTools gives history_search and history_read over the session,
	// folded history included.
	HistoryTools bool `json:"history_tools,omitempty"`
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

// Value implements driver.Valuer.
func (g MemoryGroup) Value() (driver.Value, error) { return jsonGroupValue(g) }

// Scan implements sql.Scanner.
func (g *MemoryGroup) Scan(src any) error { return jsonGroupScan(g, src) }

// Scan implements sql.Scanner.
func (g *CompactionGroup) Scan(src any) error { return jsonGroupScan(g, src) }
