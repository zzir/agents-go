package store

import (
	"context"
	"time"

	"github.com/uptrace/bun"
)

// Session is a stored conversation, optionally bound to an agent config.
type Session struct {
	bun.BaseModel `bun:"table:sessions,alias:s"`

	ID            string    `bun:"id,pk"               json:"id"`
	Name          string    `bun:"name,notnull"         json:"name"`
	AgentConfigID string    `bun:"agent_config_id"      json:"agent_config_id,omitempty"`
	CreatedAt     time.Time `bun:"created_at,notnull"   json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"   json:"updated_at"`
}

// Message is one persisted conversation item belonging to a session, holding
// both a denormalized role/content summary and the raw serialized input item.
type Message struct {
	bun.BaseModel `bun:"table:messages,alias:m"`

	ID        int64     `bun:"id,pk,autoincrement"  json:"id"`
	SessionID string    `bun:"session_id,notnull"   json:"session_id"`
	Role      string    `bun:"role,notnull"         json:"role"`
	Content   string    `bun:"content,notnull"      json:"content"`
	Item      string    `bun:"item,notnull"         json:"item,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"   json:"created_at"`
}

// AgentConfig is the persisted definition of an agent: model, instructions,
// tools, handoffs, guardrails, and the various run-level behavior settings.
type AgentConfig struct {
	bun.BaseModel `bun:"table:agent_configs,alias:ac"`

	ID            string `bun:"id,pk"                json:"id"`
	Name          string `bun:"name,notnull"          json:"name"`
	Instructions  string `bun:"instructions"          json:"instructions"`
	Model         string `bun:"model"                 json:"model"`
	ProviderType  string `bun:"provider_type"         json:"provider_type,omitempty"`
	APIKey        string `bun:"api_key"               json:"api_key,omitempty"`
	BaseURL       string `bun:"base_url"              json:"base_url,omitempty"`
	ModelSettings string `bun:"model_settings"        json:"model_settings,omitempty"`
	ToolsJSON     string `bun:"tools"                 json:"tools,omitempty"`
	HandoffsJSON  string `bun:"handoffs"              json:"handoffs,omitempty"`

	// Batch 1: basic correctness
	MaxTurns               int    `bun:"max_turns"                json:"max_turns"`
	HandoffDescription     string `bun:"handoff_description"      json:"handoff_description,omitempty"`
	DisableToolChoiceReset bool   `bun:"disable_tool_choice_reset" json:"disable_tool_choice_reset"`
	ToolUseBehavior        string `bun:"tool_use_behavior"        json:"tool_use_behavior,omitempty"`

	// Batch 2: model resilience
	RetryEnabled   bool   `bun:"retry_enabled"    json:"retry_enabled"`
	RetryPolicy    string `bun:"retry_policy"     json:"retry_policy,omitempty"`
	FallbackModels string `bun:"fallback_models"  json:"fallback_models,omitempty"`

	// Batch 3: guardrails + output type
	InputGuardrails  string `bun:"input_guardrails"  json:"input_guardrails,omitempty"`
	OutputGuardrails string `bun:"output_guardrails" json:"output_guardrails,omitempty"`
	OutputSchema     string `bun:"output_schema"     json:"output_schema,omitempty"`

	// Batch 4: session features
	UsePreviousResponseID bool   `bun:"use_previous_response_id" json:"use_previous_response_id"`
	PromptID              string `bun:"prompt_id"                json:"prompt_id,omitempty"`
	PromptVersion         string `bun:"prompt_version"           json:"prompt_version,omitempty"`

	// Batch 5: fine-grained control
	HandoffInputFilter   string `bun:"handoff_input_filter"     json:"handoff_input_filter,omitempty"`
	MaxToolConcurrency   int    `bun:"max_tool_concurrency"     json:"max_tool_concurrency"`
	ToolNotFoundBehavior string `bun:"tool_not_found_behavior"  json:"tool_not_found_behavior,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull"    json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"    json:"updated_at"`
}

// McpServerConfig is the persisted connection definition for an MCP server
// (stdio, SSE, or streamable HTTP transport).
type McpServerConfig struct {
	bun.BaseModel `bun:"table:mcp_servers,alias:ms"`

	ID            string    `bun:"id,pk"                 json:"id"`
	Name          string    `bun:"name,notnull"           json:"name"`
	TransportType string    `bun:"transport_type,notnull" json:"transport_type"`
	Command       string    `bun:"command"                json:"command,omitempty"`
	Args          string    `bun:"args"                   json:"args,omitempty"`
	Endpoint      string    `bun:"endpoint"               json:"endpoint,omitempty"`
	OptionsJSON   string    `bun:"options"                json:"options,omitempty"`
	AutoConnect   bool      `bun:"auto_connect"           json:"auto_connect"`
	CreatedAt     time.Time `bun:"created_at,notnull"     json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"     json:"updated_at"`
}

// Memory is a stored key/content fact, either global or scoped to an agent config.
type Memory struct {
	bun.BaseModel `bun:"table:memories,alias:mem"`

	ID            string    `bun:"id,pk"               json:"id"`
	AgentConfigID string    `bun:"agent_config_id"     json:"agent_config_id,omitempty"`
	Key           string    `bun:"key,notnull"          json:"key"`
	Content       string    `bun:"content,notnull"      json:"content"`
	Metadata      string    `bun:"metadata"             json:"metadata,omitempty"`
	CreatedAt     time.Time `bun:"created_at,notnull"   json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"   json:"updated_at"`
}

// Setting is a single key/value server configuration entry.
type Setting struct {
	bun.BaseModel `bun:"table:settings,alias:st"`

	Key   string `bun:"key,pk"    json:"key"`
	Value string `bun:"value"     json:"value"`
}

// ProviderRoute maps a model-name prefix to the API key and base URL used to
// reach that provider.
type ProviderRoute struct {
	bun.BaseModel `bun:"table:provider_routes,alias:pr"`

	ID        string    `bun:"id,pk"              json:"id"`
	Prefix    string    `bun:"prefix,notnull"     json:"prefix"`
	APIKey    string    `bun:"api_key"            json:"api_key,omitempty"`
	BaseURL   string    `bun:"base_url"           json:"base_url,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// TraceEvent is one persisted tracing record (a trace or span) for a session run.
type TraceEvent struct {
	bun.BaseModel `bun:"table:trace_events,alias:te"`

	ID        int64     `bun:"id,pk,autoincrement"  json:"id"`
	SessionID string    `bun:"session_id,notnull"   json:"session_id"`
	RunID     string    `bun:"run_id,notnull"       json:"run_id"`
	Kind      string    `bun:"kind,notnull"         json:"kind"`
	SpanID    string    `bun:"span_id"              json:"span_id,omitempty"`
	ParentID  string    `bun:"parent_id"            json:"parent_id,omitempty"`
	Name      string    `bun:"name,notnull"         json:"name"`
	Detail    string    `bun:"detail"               json:"detail,omitempty"`
	Data      string    `bun:"data"                 json:"data,omitempty"`
	StartedAt string    `bun:"started_at"           json:"started_at,omitempty"`
	EndedAt   string    `bun:"ended_at"             json:"ended_at,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"   json:"created_at"`
}

// SandboxConfig is the persisted definition of a code-execution sandbox backend.
type SandboxConfig struct {
	bun.BaseModel `bun:"table:sandbox_configs,alias:sb"`

	ID        string    `bun:"id,pk"                json:"id"`
	Name      string    `bun:"name,notnull"          json:"name"`
	Type      string    `bun:"type,notnull"          json:"type"`
	Host      string    `bun:"host"                  json:"host"`
	Image     string    `bun:"image"                 json:"image"`
	Network   bool      `bun:"network"               json:"network"`
	RunCmd    string    `bun:"run_cmd"               json:"run_cmd"`
	Filename  string    `bun:"filename"              json:"filename"`
	Timeout   int       `bun:"timeout"               json:"timeout"`
	CreatedAt time.Time `bun:"created_at,notnull"    json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"    json:"updated_at"`
}

// BeforeAppendModel hooks stamp id/timestamps for the CrudStore-backed entities
// (see stampOnAppend). bun invokes them on insert and update.

// BeforeAppendModel assigns an id and timestamps on insert and refreshes updated_at on update.
func (m *AgentConfig) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel assigns an id and timestamps on insert and refreshes updated_at on update.
func (m *McpServerConfig) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel assigns an id and timestamps on insert and refreshes updated_at on update.
func (m *Memory) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel assigns an id and timestamps on insert and refreshes updated_at on update.
func (m *ProviderRoute) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel assigns an id and timestamps on insert and refreshes updated_at on update.
func (m *SandboxConfig) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}
