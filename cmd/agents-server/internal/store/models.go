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

	ID string `bun:"id,pk"               json:"id"`
	// Gen names which generation of this id owns the session's entries; see
	// agents.SessionRef.
	Gen    string `bun:"gen,notnull"          json:"-"`
	Name   string `bun:"name,notnull"         json:"name"`
	Pinned bool   `bun:"pinned"               json:"pinned"`
	// Hidden marks a session that exists to serve another one — a background
	// task's transcript. Listings leave it out by default.
	//
	// It is a column rather than a subquery over the tasks table because
	// "hidden" belongs to the session: the list query had to know what a task
	// was in order to exclude one, and anything else worth hiding would have
	// had to teach it a second special case.
	Hidden        bool      `bun:"hidden"               json:"hidden,omitempty"`
	AgentConfigID string    `bun:"agent_config_id"      json:"agent_config_id,omitempty"`
	CreatedAt     time.Time `bun:"created_at,notnull"   json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"   json:"updated_at"`
}

// Task is a background subagent run spawned from a chat session via the
// spawn_task tool. Its transcript lives in a hidden child session
// (ChildSessionID); the parent linkage and terminal outcome live here. The id
// doubles as the task's run id in the hub. Status uses the MCP Tasks
// five-state vocabulary (protocol.Task*).
type Task struct {
	bun.BaseModel `bun:"table:tasks,alias:t"`

	ID string `bun:"id,pk"                json:"task_id"`
	// RunID is the id of the task's current run attempt. Today a task has
	// exactly one attempt (retries would update this and add an attempts
	// table); it is deliberately distinct from the task id so the public
	// model does not lock the two together.
	RunID           string `bun:"run_id"               json:"run_id,omitempty"`
	ParentSessionID string `bun:"parent_session_id,notnull" json:"parent_session_id"`
	// ParentSessionGen and ChildSessionGen are the GENERATIONS of the sessions
	// this row names (agents.SessionRef). A session id names a session, not a
	// place, so a row matched on the id alone attaches itself to a replacement
	// created under the same name — listing a dead incarnation's tasks under
	// the new one and owing it wake-ups it never asked for. The store binds
	// them at insert and every by-session read compares them against the
	// generation answering to that id now (see liveParent / liveChild).
	ParentSessionGen string `bun:"parent_session_gen" json:"-"`
	ParentRunID      string `bun:"parent_run_id"        json:"parent_run_id,omitempty"`
	ToolCallID       string `bun:"tool_call_id"         json:"tool_call_id,omitempty"`
	Label            string `bun:"label"                json:"label,omitempty"`
	AgentConfigID    string `bun:"agent_config_id"      json:"agent_config_id,omitempty"`
	ChildSessionID   string `bun:"child_session_id,notnull" json:"child_session_id"`
	ChildSessionGen  string `bun:"child_session_gen"        json:"-"`
	// Depth is how many task hops from a user-initiated run. Persisted because
	// it is the ONLY input to the recursion bound: dropping it made every task
	// report depth 0, so MaxDepth — the documented backstop against a task
	// spawning tasks forever — could never trip on this host.
	Depth int `bun:"depth" json:"depth,omitempty"`
	// ParentAgentConfigID / ParentSandboxID snapshot the spawning run's
	// configuration so the completion notification can start a parent run with
	// the same setup.
	ParentAgentConfigID string `bun:"parent_agent_config_id" json:"-"`
	ParentSandboxID     string `bun:"parent_sandbox_id"      json:"-"`
	Status              string `bun:"status,notnull"     json:"status"`
	Summary             string `bun:"summary,nullzero"   json:"summary,omitempty"`
	// Result is the task's full final output. The row summary (and the wake
	// notification) stay truncated to keep prompts and lists lean; the parent
	// model pulls this on demand through task_status.
	Result string `bun:"result,nullzero" json:"-"`
	// NotifyState tracks the completion wake-up owed to the parent session:
	// "" (none yet) -> "pending" (terminal result written, wake-up owed) ->
	// "consumed" (model pulled the result in-turn via task_status) or
	// "delivered" (wake-up run injected). Persisted so the auto-wake
	// survives restarts.
	NotifyState string    `bun:"notify_state,nullzero" json:"-"`
	CreatedAt   time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt   time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// AgentConfig is the persisted definition of an agent: model, instructions,
// tools, handoffs, guardrails, and the various run-level behavior settings.
type AgentConfig struct {
	bun.BaseModel `bun:"table:agent_configs,alias:ac"`

	ID           string `bun:"id,pk"          json:"id"`
	Name         string `bun:"name,notnull"   json:"name"`
	Instructions string `bun:"instructions"   json:"instructions"`
	Model        string `bun:"model"          json:"model"`

	// The remaining knobs are grouped into JSON category columns (see
	// agent_config_groups.go) so the table holds only category columns and a new
	// setting needs no schema change. In the REST API each is a nested object.
	Provider   ProviderGroup   `bun:"provider,type:text,nullzero"   json:"provider"`
	Behavior   BehaviorGroup   `bun:"behavior,type:text,nullzero"   json:"behavior"`
	Resilience ResilienceGroup `bun:"resilience,type:text,nullzero" json:"resilience"`
	Guardrails GuardrailGroup  `bun:"guardrails,type:text,nullzero" json:"guardrails"`
	Session    SessionGroup    `bun:"session,type:text,nullzero"    json:"session"`
	Approval   ApprovalGroup   `bun:"approval,type:text,nullzero"   json:"approval"`
	Compaction CompactionGroup `bun:"compaction,type:text,nullzero" json:"compaction"`

	// The following are already single JSON blobs, kept as their own columns.
	ModelSettings string `bun:"model_settings" json:"model_settings,omitempty"`
	ToolsJSON     string `bun:"tools"          json:"tools,omitempty"`
	SkillsJSON    string `bun:"skills"         json:"skills,omitempty"`
	HandoffsJSON  string `bun:"handoffs"       json:"handoffs,omitempty"`
	// ErrorHandlers is a JSON object keyed by error kind (max_turns /
	// model_refusal / invalid_final_output), each entry carrying a static
	// final_output (a JSON value) and an optional exclude_from_history flag.
	// Empty means every run error stays fatal.
	ErrorHandlers string `bun:"error_handlers" json:"error_handlers,omitempty"`

	// ChatGPT OAuth token (JSON-serialized). Never serialized to the API
	// (json:"-"); preserved across regular CRUD updates (the store excludes the
	// column) so editing an agent doesn't erase its token.
	ChatGPTToken string `bun:"chatgpt_token,type:text,nullzero" json:"-"`
	// ChatGPTLoggedIn is the API-facing derived login signal (set by the
	// handler when sanitizing); the token itself never leaves the server.
	ChatGPTLoggedIn bool `bun:"-" json:"chatgpt_logged_in,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// McpServerConfig is the persisted connection definition for an MCP server.
// Transport-specific settings live in Config (JSON, interpreted per
// TransportType), so a new transport needs no schema migration.
type McpServerConfig struct {
	bun.BaseModel `bun:"table:mcp_servers,alias:ms"`

	ID            string `bun:"id,pk"                  json:"id"`
	Name          string `bun:"name,notnull"           json:"name"`
	TransportType string `bun:"transport_type,notnull" json:"transport_type"` // stdio | streamable_http
	// Enabled deliberately carries no bun default tag: with `default:true`,
	// bun swaps a zero-value false for SQL DEFAULT on insert, silently
	// enabling a server that was created with enabled=false.
	Enabled bool `bun:"enabled,notnull"        json:"enabled"`

	// Config holds the transport-specific settings as JSON: StdioMcpConfig for
	// "stdio", HTTPMcpConfig for "streamable_http". Stored as TEXT and
	// exchanged with the API as a raw JSON object.
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	// OAuthToken is the JSON-serialized OAuth grant obtained during the OAuth
	// flow: the oauth2.Token plus the token endpoint and client credentials
	// needed to refresh it across restarts (bridge.tokenPayload). Stored
	// separately from Config so that regular CRUD updates (which overwrite
	// Config) don't erase it, and hidden from the API (json:"-").
	OAuthToken string `bun:"oauth_token,type:text,nullzero" json:"-"`

	CreatedAt time.Time `bun:"created_at,notnull"     json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"     json:"updated_at"`
}

// McpRetryConfig holds the per-request retry settings common to every MCP
// transport, embedded in the transport-specific config payloads. A single
// transient failure on list_tools/call_tool otherwise aborts the whole run.
type McpRetryConfig struct {
	// MaxRetryAttempts retries a failed list_tools/call_tool this many times.
	// 0 (default) disables retries; -1 retries indefinitely.
	MaxRetryAttempts int `json:"max_retry_attempts,omitempty"`
	// RetryBackoffMs is the base delay (milliseconds) for exponential backoff
	// between retries. 0 leaves the SDK default (1s) when retries are enabled.
	RetryBackoffMs int `json:"retry_backoff_ms,omitempty"`
}

// StdioMcpConfig is the McpServerConfig.Config payload for TransportType == "stdio".
type StdioMcpConfig struct {
	Command        string   `json:"command"`
	Args           []string `json:"args,omitempty"`
	McpRetryConfig          // max_retry_attempts / retry_backoff_ms
	// UseStructuredContent uses a tool result's structuredContent field
	// exclusively (default: use the content blocks). For servers that only
	// populate the structured field.
	UseStructuredContent bool `json:"use_structured_content,omitempty"`
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

	McpRetryConfig // max_retry_attempts / retry_backoff_ms
	// UseStructuredContent uses a tool result's structuredContent exclusively
	// (default: the content blocks). See StdioMcpConfig.UseStructuredContent.
	UseStructuredContent bool `json:"use_structured_content,omitempty"`
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

// ProviderRoute maps a model-name prefix to the backend, API key and base URL
// used to reach that provider.
type ProviderRoute struct {
	bun.BaseModel `bun:"table:provider_routes,alias:pr"`

	ID     string `bun:"id,pk"          json:"id"`
	Prefix string `bun:"prefix,notnull" json:"prefix"`
	// ProviderType selects the backend ("openai" / "anthropic"); empty is
	// openai.
	ProviderType string    `bun:"provider_type"      json:"provider_type,omitempty"`
	APIKey       string    `bun:"api_key"            json:"api_key,omitempty"`
	BaseURL      string    `bun:"base_url"           json:"base_url,omitempty"`
	CreatedAt    time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt    time.Time `bun:"updated_at,notnull" json:"updated_at"`
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

	// Terminal reports whether this sandbox can host an interactive web
	// terminal (ssh always; docker only in persistent mode; local never, by
	// design). Computed per response by the handler, never stored.
	Terminal bool `bun:"-" json:"terminal"`

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
	User             string `json:"user,omitempty"`    // user[:group] the container runs as; "" = backend default (65534 nobody)
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

	ID          string `bun:"id,pk"              json:"id"`
	Name        string `bun:"name,notnull"       json:"name"`
	Description string `bun:"description"        json:"description"`
	// Stages are the run stages this guardrail inspects: input, output,
	// tool_input, tool_output. One definition covering several is the SDK's
	// model — a content scanner that should see the input, the tool arguments
	// and the final output is one guardrail, not three near-identical copies.
	Stages []string        `bun:"stages,type:text"   json:"stages"`
	Mode   string          `bun:"mode,notnull"       json:"mode"` // regex | max_length
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`
	// Blocking, at the input stage, runs the guardrail to completion BEFORE the
	// first model call (a gate) instead of racing it — a tripwire then prevents
	// the call and any token spend. No effect at the other stages.
	Blocking  bool      `bun:"blocking" json:"blocking"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
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
	// UserInput is the text of the message that started this paused turn. The
	// SDK only persists the turn to `messages` on completion, so during the
	// pause this is the only place the user's prompt is stored — the UI
	// reconstructs the user bubble from it on reload.
	UserInput string    `bun:"user_input,type:text,nullzero" json:"user_input,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"            json:"created_at"`
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
