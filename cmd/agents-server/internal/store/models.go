package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// Session is a stored conversation, optionally bound to an agent config.
type Session struct {
	bun.BaseModel `bun:"table:sessions,alias:s"`

	ID            string    `bun:"id,pk"               json:"id"`
	Name          string    `bun:"name,notnull"         json:"name"`
	Pinned        bool      `bun:"pinned"               json:"pinned"`
	AgentConfigID string    `bun:"agent_config_id"      json:"agent_config_id,omitempty"`
	CreatedAt     time.Time `bun:"created_at,notnull"   json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"   json:"updated_at"`
}

// Message kinds. "item" rows are replayable conversation history holding wire
// JSON in Item; "annotation" rows are UI-only records (errors, partial
// reasoning from a cancelled run) that never reach the model.
const (
	MessageKindItem       = "item"
	MessageKindAnnotation = "annotation"
)

// Message is one persisted conversation record belonging to a session. For
// kind="item" rows, Item holds the canonical (write-normalized) wire JSON and
// is the replay source of truth; Role/Content/Display are denormalized
// projections of it for the UI, which never parses wire JSON itself.
type Message struct {
	bun.BaseModel `bun:"table:messages,alias:m"`

	ID        int64  `bun:"id,pk,autoincrement"  json:"id"`
	SessionID string `bun:"session_id,notnull"   json:"session_id"`
	RunID     string `bun:"run_id"               json:"run_id,omitempty"`
	Kind      string `bun:"kind"                 json:"kind,omitempty"`
	Role      string `bun:"role,notnull"         json:"role"`
	Content   string `bun:"content,notnull"      json:"content"`
	// Display carries the structured fields the UI renders for non-text rows
	// (tool call name/arguments/output), derived from Item at write time.
	Display json.RawMessage `bun:"display,type:text,nullzero" json:"display,omitempty"`
	Item    string          `bun:"item,notnull"               json:"-"`
	// SourceModel records which model produced this item, so replay can adapt
	// or drop items when the session is later run against a different model.
	SourceModel string    `bun:"source_model"       json:"-"`
	Compacted   bool      `bun:"compacted"          json:"compacted"`
	CreatedAt   time.Time `bun:"created_at,notnull" json:"created_at"`
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
	AuthMode      string `bun:"auth_mode"             json:"auth_mode,omitempty"`
	APIKey        string `bun:"api_key"               json:"api_key,omitempty"`
	BaseURL       string `bun:"base_url"              json:"base_url,omitempty"`
	ModelSettings string `bun:"model_settings"        json:"model_settings,omitempty"`
	ToolsJSON     string `bun:"tools"                 json:"tools,omitempty"`
	SkillsJSON    string `bun:"skills"                json:"skills,omitempty"`
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

	// ChatGPT OAuth token (JSON-serialized). Hidden from API; preserved across
	// regular CRUD updates so editing an agent doesn't erase its token.
	ChatGPTToken string `bun:"chatgpt_token,type:text,nullzero" json:"-"`

	// Batch 5: fine-grained control
	HandoffInputFilter   string `bun:"handoff_input_filter"     json:"handoff_input_filter,omitempty"`
	MaxToolConcurrency   int    `bun:"max_tool_concurrency"     json:"max_tool_concurrency"`
	ToolNotFoundBehavior string `bun:"tool_not_found_behavior"  json:"tool_not_found_behavior,omitempty"`

	// Batch 6: HITL approval
	ApproveTools string `bun:"approve_tools" json:"approve_tools,omitempty"` // JSON: ["*"] or ["tool_name",...]

	// Batch 7: compaction
	CompactionEnabled   bool   `bun:"compaction_enabled"   json:"compaction_enabled"`
	CompactionThreshold int    `bun:"compaction_threshold" json:"compaction_threshold"`
	CompactionWindow    int    `bun:"compaction_window"    json:"compaction_window"`
	CompactionModel     string `bun:"compaction_model"     json:"compaction_model,omitempty"`
	CompactionPrompt    string `bun:"compaction_prompt"    json:"compaction_prompt,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull"    json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"    json:"updated_at"`
}

// McpServerConfig is the persisted connection definition for an MCP server.
// Transport-specific settings live in Config (JSON, interpreted per
// TransportType), so a new transport needs no schema migration.
type McpServerConfig struct {
	bun.BaseModel `bun:"table:mcp_servers,alias:ms"`

	ID            string `bun:"id,pk"                  json:"id"`
	Name          string `bun:"name,notnull"           json:"name"`
	TransportType string `bun:"transport_type,notnull" json:"transport_type"` // stdio | streamable_http
	Enabled       bool   `bun:"enabled,default:true"    json:"enabled"`

	// Config holds the transport-specific settings as JSON: StdioMcpConfig for
	// "stdio", HTTPMcpConfig for "streamable_http". Stored as TEXT and
	// exchanged with the API as a raw JSON object.
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	// OAuthToken is the JSON-serialized oauth2.Token obtained during the OAuth
	// flow. Stored separately from Config so that regular CRUD updates (which
	// overwrite Config) don't erase it, and hidden from the API (json:"-").
	OAuthToken string `bun:"oauth_token,type:text,nullzero" json:"-"`

	CreatedAt time.Time `bun:"created_at,notnull"     json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"     json:"updated_at"`
}

// StdioMcpConfig is the McpServerConfig.Config payload for TransportType == "stdio".
type StdioMcpConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// HTTPMcpConfig is the McpServerConfig.Config payload for the "streamable_http"
// transport.
type HTTPMcpConfig struct {
	Endpoint string `json:"endpoint"`
	// Headers are added to every HTTP request to the server, e.g. an
	// "Authorization: Bearer <token>" or an API-key header.
	Headers map[string]string `json:"headers,omitempty"`

	// AuthMode selects the authentication method: "" or "header" for static
	// headers (the default), "oauth" for the OAuth 2.1 authorization code flow.
	AuthMode string `json:"auth_mode,omitempty"`
	// OAuthClientID is an optional pre-registered client ID. When empty and
	// AuthMode is "oauth", the server will use dynamic client registration.
	OAuthClientID string `json:"oauth_client_id,omitempty"`
	// OAuthClientSecret is the corresponding client secret (if pre-registered).
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	// OAuthScopes are the OAuth scopes to request during authorization.
	OAuthScopes string `json:"oauth_scopes,omitempty"`
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
	Error     string    `bun:"error"                json:"error,omitempty"`
	Data      string    `bun:"data"                 json:"data,omitempty"`
	StartedAt string    `bun:"started_at"           json:"started_at,omitempty"`
	EndedAt   string    `bun:"ended_at"             json:"ended_at,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"   json:"created_at"`
}

// SandboxConfig is the persisted definition of a code-execution sandbox backend.
// Backend-specific settings live in Config (JSON, interpreted per Type), so a new
// backend type needs no schema migration — only a new Config payload struct and a
// case in the bridge.
type SandboxConfig struct {
	bun.BaseModel `bun:"table:sandbox_configs,alias:sb"`

	ID   string `bun:"id,pk"        json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	Type string `bun:"type,notnull" json:"type"` // local | docker | ssh

	// Config holds the backend-specific settings as JSON: LocalConfig for
	// "local", DockerConfig for "docker", SSHConfig for "ssh". Stored as TEXT
	// and sent to/received from the API as a raw JSON object (no
	// double-encoding).
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// LocalConfig is the SandboxConfig.Config payload for Type == "local". It may
// be empty: every field has a working default.
type LocalConfig struct {
	MaxReadFileBytes int64 `json:"max_read_file_bytes,omitempty"` // read_file cap in bytes; 0 = backend default (8 MiB)
}

// DockerConfig is the SandboxConfig.Config payload for Type == "docker".
type DockerConfig struct {
	Image            string `json:"image"`
	Runtime          string `json:"runtime,omitempty"` // OCI runtime (e.g. "runsc" for gVisor)
	Network          bool   `json:"network"`
	Persistent       bool   `json:"persistent"`
	ContainerName    string `json:"container_name,omitempty"`      // Docker container name (persistent mode only)
	MaxReadFileBytes int64  `json:"max_read_file_bytes,omitempty"` // read_file cap in bytes; 0 = backend default (8 MiB)
}

// SSHConfig is the SandboxConfig.Config payload for Type == "ssh".
type SSHConfig struct {
	Addr             string `json:"addr"` // remote host[:port]
	User             string `json:"user"`
	UseAgent         bool   `json:"use_agent"`
	KeyFile          string `json:"key_file,omitempty"`
	Password         string `json:"password,omitempty"`
	KnownHosts       string `json:"known_hosts,omitempty"`
	InsecureHostKey  bool   `json:"insecure_host_key"`
	WorkDir          string `json:"work_dir,omitempty"`            // fixed remote working directory
	MaxReadFileBytes int64  `json:"max_read_file_bytes,omitempty"` // read_file cap in bytes; 0 = backend default (8 MiB)
}

// BeforeAppendModel hooks stamp id/timestamps for the CrudStore-backed entities
// (see stampOnAppend). bun invokes them on insert and update.

// Guardrail is a stored guardrail definition. Mode selects the check logic:
// "regex" uses Config.Pattern; "max_length" uses Config.MaxLength.
type Guardrail struct {
	bun.BaseModel `bun:"table:guardrails,alias:gr"`

	ID          string          `bun:"id,pk"              json:"id"`
	Name        string          `bun:"name,notnull"       json:"name"`
	Description string          `bun:"description"        json:"description"`
	Type        string          `bun:"type,notnull"       json:"type"` // input | output
	Mode        string          `bun:"mode,notnull"       json:"mode"` // regex | max_length
	Config      json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`
	CreatedAt   time.Time       `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt   time.Time       `bun:"updated_at,notnull" json:"updated_at"`
}

// GuardrailConfig is the parsed Config payload for a Guardrail.
type GuardrailConfig struct {
	Pattern   string `json:"pattern,omitempty"`
	MaxLength int    `json:"max_length,omitempty"`
}

// PendingApproval is a run paused for human-in-the-loop tool approval,
// persisted so it survives process restarts and is addressable over REST. The
// serialized SDK RunState is the resume source of truth; ToolCalls is a
// UI-facing projection of the interruptions awaiting a decision.
type PendingApproval struct {
	bun.BaseModel `bun:"table:pending_approvals,alias:pa"`

	RunID         string `bun:"run_id,pk"              json:"run_id"`
	SessionID     string `bun:"session_id,notnull"     json:"session_id"`
	AgentConfigID string `bun:"agent_config_id"        json:"agent_config_id,omitempty"`
	SandboxID     string `bun:"sandbox_id"             json:"sandbox_id,omitempty"`
	// State is the JSON from agents.RunState.MarshalJSON. Hidden from the API.
	State string `bun:"state,type:text,notnull" json:"-"`
	// ToolCalls is the JSON array of pending tool calls ([]PendingToolCall)
	// shown to the user.
	ToolCalls json.RawMessage `bun:"tool_calls,type:text,nullzero" json:"tool_calls,omitempty"`
	CreatedAt time.Time       `bun:"created_at,notnull"            json:"created_at"`
}

// PendingToolCall is one tool call awaiting approval, projected from a run's
// interruptions for the approvals listing.
type PendingToolCall struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Arguments  string `json:"arguments"`
}

// parsedToolCalls decodes the ToolCalls JSON, returning nil on malformed data.
func (p *PendingApproval) parsedToolCalls() []PendingToolCall {
	if len(p.ToolCalls) == 0 {
		return nil
	}
	var out []PendingToolCall
	if err := json.Unmarshal(p.ToolCalls, &out); err != nil {
		return nil
	}
	return out
}

// BeforeAppendModel assigns an id and timestamps on insert and refreshes updated_at on update.
func (m *Guardrail) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

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
