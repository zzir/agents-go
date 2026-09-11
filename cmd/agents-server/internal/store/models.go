package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// Session is a stored conversation, optionally bound to an agent config.
type Session struct {
	bun.BaseModel `bun:"table:sessions,alias:s"`

	ID string `bun:"id,pk,type:uuid"     json:"id"`
	// Gen is the generation of this id that owns the session's entries (session.Ref).
	Gen string `bun:"gen,notnull"          json:"-"`
	// OwnerID is the user the conversation belongs to; a task's hidden session inherits its parent's.
	OwnerID string `bun:"owner_id,notnull,type:uuid" json:"owner_id"`
	Name    string `bun:"name,notnull"         json:"name"`
	Pinned  bool   `bun:"pinned"               json:"pinned"`
	// Hidden marks a background task's transcript session; listings leave it out.
	Hidden        bool   `bun:"hidden"               json:"hidden,omitempty"`
	AgentConfigID string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	// ProjectID is the project the session is bound to, set once by the first project-carrying run and never rewritten.
	ProjectID string `bun:"project_id,nullzero,type:uuid" json:"project_id,omitempty"`
	// Planning is the session's plan phase: the next run starts read-only until a plan is approved.
	Planning  bool      `bun:"planning"             json:"planning"`
	CreatedAt time.Time `bun:"created_at,notnull"   json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"   json:"updated_at"`
}

// Task is one piece of background work spawned from a chat session through
// spawn_task: a sub-agent task, or a workflow execution (Kind
// TaskKindWorkflow). Its transcript lives in a hidden child session
// (ChildSessionID); the parent linkage and terminal outcome live here. Status
// uses the MCP Tasks five-state vocabulary (protocol.Task*).
type Task struct {
	bun.BaseModel `bun:"table:tasks,alias:t"`

	ID string `bun:"id,pk,type:uuid"      json:"task_id"`
	// RunID is the task's current run; a retry and each workflow step replace it.
	RunID string `bun:"run_id,nullzero,type:uuid" json:"run_id,omitempty"`
	// Kind is "" for a sub-agent task, TaskKindWorkflow for a workflow execution.
	Kind string `bun:"kind" json:"kind,omitempty"`
	// State is the SDK's opaque per-job record; a workflow's is its encoded WorkflowState.
	State           json.RawMessage `bun:"state,type:text,nullzero" json:"state,omitempty"`
	ParentSessionID string          `bun:"parent_session_id,notnull,type:uuid" json:"parent_session_id"`
	// ParentSessionGen and ChildSessionGen bind the row to the session generations it names; by-session reads compare them.
	ParentSessionGen string `bun:"parent_session_gen" json:"-"`
	ParentRunID      string `bun:"parent_run_id,nullzero,type:uuid" json:"parent_run_id,omitempty"`
	ToolCallID       string `bun:"tool_call_id"         json:"tool_call_id,omitempty"`
	Label            string `bun:"label"                json:"label,omitempty"`
	AgentConfigID    string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	ChildSessionID   string `bun:"child_session_id,notnull,type:uuid" json:"child_session_id"`
	ChildSessionGen  string `bun:"child_session_gen"        json:"-"`
	// Depth is how many task hops from a user-initiated run; the recursion bound (MaxDepth) reads it.
	Depth int `bun:"depth" json:"depth,omitempty"`
	// Attempt counts this task's runs, 1 for the original; zero reads as the first.
	Attempt int `bun:"attempt" json:"attempt,omitempty"`
	// ParentAgentConfigID and ParentProjectID snapshot the spawning run's configuration for the wake-up run and a retry.
	ParentAgentConfigID string `bun:"parent_agent_config_id,nullzero,type:uuid" json:"-"`
	ParentProjectID     string `bun:"parent_project_id,nullzero,type:uuid" json:"-"`
	Status              string `bun:"status,notnull"     json:"status"`
	Summary             string `bun:"summary,nullzero"   json:"summary,omitempty"`
	// Result is the task's full final output; Summary and the wake-up notification stay truncated.
	Result string `bun:"result,nullzero" json:"-"`
	// Dismissed hides a terminal task from the conversation's live strip; a retry clears it.
	Dismissed bool `bun:"dismissed" json:"dismissed,omitempty"`
	// MaxAttempts is the task manager's ceiling on Attempt, filled for the wire and not stored.
	MaxAttempts int       `bun:"-" json:"max_attempts,omitempty"`
	CreatedAt   time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt   time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// TaskKindWorkflow is the Task.Kind of a workflow execution.
const TaskKindWorkflow = "workflow"

// AgentConfig is the persisted definition of an agent: model, instructions,
// tools, handoffs, guardrails, and the various run-level behavior settings.
type AgentConfig struct {
	bun.BaseModel `bun:"table:agent_configs,alias:ac"`

	ID   string `bun:"id,pk,type:uuid" json:"id"`
	Name string `bun:"name,notnull"   json:"name"`
	// Description is what the agent is for, in a sentence; an agent picker matches requests against it.
	Description string `bun:"description,nullzero" json:"description,omitempty"`
	// Avatar is a same-origin path into the built-in catalog ("/avatars/<name>.svg"); empty renders an initial.
	Avatar       string `bun:"avatar,nullzero"      json:"avatar,omitempty"`
	Instructions string `bun:"instructions"   json:"instructions"`
	Model        string `bun:"model"          json:"model"`
	// ProviderID names the Provider the agent reaches its model through; empty fails the run's pre-flight.
	ProviderID string `bun:"provider_id,nullzero,type:uuid" json:"provider_id,omitempty"`
	// ContextWindow is the model's window in tokens, declared per agent; 0 leaves the Context panel without a denominator.
	ContextWindow int `bun:"context_window" json:"context_window,omitempty"`

	// The run-level settings, one JSON column per category (agent_config_groups.go); each a nested object in the API.
	Behavior   BehaviorGroup   `bun:"behavior,type:text,nullzero"   json:"behavior"`
	Resilience ResilienceGroup `bun:"resilience,type:text,nullzero" json:"resilience"`
	Guardrails GuardrailGroup  `bun:"guardrails,type:text,nullzero" json:"guardrails"`
	Session    SessionGroup    `bun:"session,type:text,nullzero"    json:"session"`
	Approval   ApprovalGroup   `bun:"approval,type:text,nullzero"   json:"approval"`
	Compaction CompactionGroup `bun:"compaction,type:text,nullzero" json:"compaction"`
	Memory     MemoryGroup     `bun:"memory,type:text,nullzero"     json:"memory"`

	// ModelSettings is a JSON object of model parameters (temperature, reasoning, extra_body, ...).
	ModelSettings string `bun:"model_settings" json:"model_settings,omitempty"`
	// Tools lists the ids of the MCP servers whose tools the agent carries.
	Tools StringList `bun:"tools,type:text" json:"tools,omitempty"`
	// Skills lists the ids of the skills the agent may read; null means every skill its scope can see, [] none.
	Skills StringList `bun:"skills,type:text" json:"skills"`
	// Handoffs lists the ids of the agents this one can hand off to.
	Handoffs StringList `bun:"handoffs,type:text" json:"handoffs,omitempty"`
	// ErrorHandlers is a JSON object keyed by error kind (max_turns, model_refusal, invalid_final_output); empty keeps every run error fatal.
	ErrorHandlers string `bun:"error_handlers" json:"error_handlers,omitempty"`

	// Scope/OwnerID: row visibility and its permanent creator.
	Scope   string `bun:"scope,notnull"                 json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid"   json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// Provider is one configured backend endpoint and the credential that reaches
// it — the single place a model-API key lives; agents reference it by id
// (decisions §5.30).
type Provider struct {
	bun.BaseModel `bun:"table:providers,alias:pv"`

	ID   string `bun:"id,pk,type:uuid" json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	// Type selects the backend (bridge.ProviderType*); empty means openai.
	Type string `bun:"type"      json:"type,omitempty"`
	// AuthMode is "" (API key) or a mode the backend offers, validated on save.
	AuthMode string `bun:"auth_mode" json:"auth_mode,omitempty"`
	// APIKey is masked on the way out and restored from the stored row when the mask is sent back.
	APIKey  string `bun:"api_key"   json:"api_key,omitempty"`
	BaseURL string `bun:"base_url"  json:"base_url,omitempty"`
	// ChatGPTToken is the serialized OAuth token for auth_mode chatgpt_login; never serialized, kept across updates.
	ChatGPTToken string `bun:"chatgpt_token,type:text,nullzero" json:"-"`
	// ChatGPTLoggedIn is derived when sanitizing: whether a ChatGPT token is stored.
	ChatGPTLoggedIn bool `bun:"-" json:"chatgpt_logged_in,omitempty"`

	// Scope/OwnerID: row visibility and its permanent creator.
	Scope   string `bun:"scope,notnull"                 json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid"   json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// McpServerConfig is the persisted connection definition for an MCP server
// (streamable HTTP only — decisions §5.25); connection settings live in Config.
type McpServerConfig struct {
	bun.BaseModel `bun:"table:mcp_servers,alias:ms"`

	ID   string `bun:"id,pk,type:uuid"        json:"id"`
	Name string `bun:"name,notnull"           json:"name"`
	// Enabled carries no bun default tag: with one, bun would write SQL DEFAULT for a false on insert.
	Enabled bool `bun:"enabled,notnull"        json:"enabled"`

	// Config holds the connection settings as JSON (HTTPMcpConfig), exchanged with the API as a raw object.
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	// OAuthToken is the JSON OAuth grant with what refreshes it (bridge.tokenPayload), kept apart from Config and off the API.
	OAuthToken string `bun:"oauth_token,type:text,nullzero" json:"-"`

	// Scope/OwnerID: row visibility and its permanent creator.
	Scope   string `bun:"scope,notnull"                 json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid"   json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull"     json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"     json:"updated_at"`
}

// McpRetryConfig holds the per-request retry settings, embedded in
// HTTPMcpConfig. A single transient failure on list_tools/call_tool
// otherwise aborts the whole run.
type McpRetryConfig struct {
	// MaxRetryAttempts retries a failed list_tools/call_tool; 0 disables, -1 retries indefinitely.
	MaxRetryAttempts int `json:"max_retry_attempts,omitempty"`
	// RetryBackoffMs is the base delay for exponential backoff; 0 leaves the SDK default (1s).
	RetryBackoffMs int `json:"retry_backoff_ms,omitempty"`
}

// HTTPMcpConfig is the McpServerConfig.Config payload (streamable HTTP).
type HTTPMcpConfig struct {
	Endpoint string `json:"endpoint"`
	// Headers are added to every request, e.g. an Authorization or API-key header.
	Headers map[string]string `json:"headers,omitempty"`

	// AuthMode is "" or "header" for static headers, "oauth" for the OAuth 2.1 authorization code flow.
	AuthMode string `json:"auth_mode,omitempty"`
	// OAuthClientID is a pre-registered client id; empty uses dynamic client registration.
	OAuthClientID string `json:"oauth_client_id,omitempty"`
	// OAuthClientSecret is the corresponding client secret (if pre-registered).
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	// OAuthScopes are the OAuth scopes to request during authorization.
	OAuthScopes string `json:"oauth_scopes,omitempty"`

	McpRetryConfig // max_retry_attempts / retry_backoff_ms
	// UseStructuredContent takes a tool result's structuredContent alone, for servers that fill only that field.
	UseStructuredContent bool `json:"use_structured_content,omitempty"`
}

// Skill is one stored SKILL.md document (decisions §5.26). Name and Description
// are denormalized from the content's frontmatter at save time — the content
// is the document, the columns are its index entry.
type Skill struct {
	bun.BaseModel `bun:"table:skills,alias:sk"`

	ID          string `bun:"id,pk,type:uuid" json:"id"`
	Name        string `bun:"name,notnull"    json:"name"` // unique per scope
	Description string `bun:"description,notnull" json:"description"`
	// Content is the full SKILL.md, capped at write time (maxSkillBytes) and omitted from list responses.
	Content string `bun:"content,notnull,type:text" json:"content,omitempty"`

	// SourceRepo, SourcePath and SourceSHA record where an import came from, for a re-import to match; empty when authored here.
	SourceRepo string `bun:"source_repo,nullzero" json:"source_repo,omitempty"`
	SourcePath string `bun:"source_path,nullzero" json:"source_path,omitempty"`
	SourceSHA  string `bun:"source_sha,nullzero"  json:"source_sha,omitempty"`
	// RepoLabel is SourceRepo reduced to the prefix the unique name indexes key on ("owner/repo" or the host), derived on write.
	RepoLabel string `bun:"repo_label,nullzero" json:"repo_label,omitempty"`
	// Detached marks an imported skill edited here; a re-import skips it.
	Detached bool `bun:"detached,notnull" json:"detached,omitempty"`

	// Scope/OwnerID: row visibility and its permanent creator.
	Scope   string `bun:"scope,notnull"                 json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid"   json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// Memory is one remembered text, keyed within its scope: what an agent reads
// with every request (the global and agent scopes) or keeps for itself
// across a conversation's compactions and resets (the session scope). The
// rules per scope are MemoryPolicies.
type Memory struct {
	bun.BaseModel `bun:"table:memories,alias:mem"`

	ID string `bun:"id,pk,type:uuid" json:"id"`
	// ScopeKind is global, agent or session; ScopeID names the agent or session, empty for global.
	ScopeKind string `bun:"scope_kind,notnull" json:"scope_kind"`
	ScopeID   string `bun:"scope_id,notnull"   json:"scope_id,omitempty"`
	// Gen is the session generation a session memory belongs to; empty otherwise.
	Gen string `bun:"gen,notnull" json:"-"`
	// Key is unique within the scope; a session memory's key is path-like.
	Key      string `bun:"key,notnull"     json:"key"`
	Content  string `bun:"content,notnull" json:"content"`
	Metadata string `bun:"metadata"        json:"metadata,omitempty"`
	// WrittenBy is user or model.
	WrittenBy string `bun:"written_by,notnull" json:"written_by"`
	// OwnerID is the user who wrote it: the caller, or the session's owner when the model did.
	OwnerID   string    `bun:"owner_id,nullzero,type:uuid" json:"owner_id,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"          json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"          json:"updated_at"`
}

// Attachment is one uploaded image: metadata only — the bytes live in the
// configured S3-compatible bucket under Key, and session entries reference
// the row by id (an "agents-attachment:<id>" sentinel URL) that is resolved
// to the bucket's public URL only at the model boundary.
type Attachment struct {
	bun.BaseModel `bun:"table:attachments,alias:att"`

	ID      string `bun:"id,pk,type:uuid"           json:"id"`
	OwnerID string `bun:"owner_id,notnull,type:uuid" json:"owner_id"`
	// Key addresses the object in the bucket; the public URL is derived from the current s3_public_base_url.
	Key  string `bun:"key,notnull"  json:"key"`
	Mime string `bun:"mime,notnull" json:"mime"`
	Size int64  `bun:"size,notnull" json:"size"`
	// Bound: set when a run accepts the attachment, cleared when the last session referencing it is deleted; unbound rows are collected.
	Bound     bool      `bun:"bound,notnull"      json:"bound"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

// ContextProfile is what a session's last build put in front of the model
// before the conversation itself — a SNAPSHOT written per run, since the
// sizes depend on what that build attached (workbench invariant 28).
type ContextProfile struct {
	bun.BaseModel `bun:"table:context_profiles,alias:cxp"`

	SessionID string `bun:"session_id,pk,type:uuid"`
	// Payload is a PromptProfile as JSON.
	Payload string `bun:"payload,type:text"`
}

// PromptProfile is the ContextProfile payload: the instruction layers and the
// tool surface, sized in CHARACTERS. Same ruler as the compaction estimate and
// NOT the provider's — see workbench invariant 28.
type PromptProfile struct {
	// The instruction layers, in the order WrapInstructions composed them.
	InstructionsChars  int `json:"instructions_chars,omitempty"`
	GlobalPromptChars  int `json:"global_prompt_chars,omitempty"`
	MemoryChars        int `json:"memory_chars,omitempty"`
	SandboxPromptChars int `json:"sandbox_prompt_chars,omitempty"`
	// ContextGuidanceChars is the memory and reset guidance the build appended.
	ContextGuidanceChars int `json:"context_guidance_chars,omitempty"`
	SkillsIndexChars     int `json:"skills_index_chars,omitempty"`
	// Tools are the locally attached tools by origin; MCP tools are sized by the read path, not here.
	Tools []ToolBucket `json:"tools,omitempty"`
	// MCPServerIDs are the servers the build wired up, in config order.
	MCPServerIDs []string `json:"mcp_server_ids,omitempty"`
}

// ToolBucket is one origin's share of the tool surface.
type ToolBucket struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
	Chars  int    `json:"chars"`
	// Unavailable marks a bucket that could not be measured (a disconnected MCP server): unknown, never zero.
	Unavailable bool `json:"unavailable,omitempty"`
}

// Tool bucket sources.
const (
	ToolSourceSandbox = "sandbox"
	ToolSourceSkills  = "skills"
	ToolSourceTasks   = "tasks"
	// ToolSourceWorkflows is the workflow-authoring pair, get_workflow and
	// save_workflow (workbench invariant 39).
	ToolSourceWorkflows = "workflows"
	ToolSourceContext   = "context"
	ToolSourceTodo      = "todo"
	ToolSourcePlan      = "plan"
	// ToolSourceMCP is a prefix: "mcp:<server name>".
	ToolSourceMCP = "mcp:"
)

// Setting is a single key/value server configuration entry.
type Setting struct {
	bun.BaseModel `bun:"table:settings,alias:st"`

	Key   string `bun:"key,pk"    json:"key"`
	Value string `bun:"value"     json:"value"`
}

// TraceEvent is one persisted tracing record (a trace or span) for a session run.
type TraceEvent struct {
	bun.BaseModel `bun:"table:trace_events,alias:te"`

	ID        string `bun:"id,pk,type:uuid"      json:"id"`
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	RunID     string `bun:"run_id,notnull,type:uuid" json:"run_id"`
	// ParentRunID is the run whose spawn started this run's chain; the panel groups runs by it.
	ParentRunID string `bun:"parent_run_id,nullzero,type:uuid" json:"parent_run_id,omitempty"`
	Kind        string `bun:"kind,notnull"         json:"kind"`
	SpanID      string `bun:"span_id"              json:"span_id,omitempty"`
	ParentID    string `bun:"parent_id"            json:"parent_id,omitempty"`
	Name        string `bun:"name,notnull"         json:"name"`
	Detail      string `bun:"detail"               json:"detail,omitempty"`
	Error       string `bun:"error"                json:"error,omitempty"`
	// Data is the span's metadata JSON; Layout and Refs (32-byte sha256 per element) address its payload in trace_blobs, NULL without one.
	Data      string    `bun:"data"                 json:"data,omitempty"`
	Layout    string    `bun:"layout,nullzero"      json:"-"`
	Refs      []byte    `bun:"refs,nullzero"        json:"-"`
	StartedAt string    `bun:"started_at"           json:"started_at,omitempty"`
	EndedAt   string    `bun:"ended_at"             json:"ended_at,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"   json:"created_at"`
	// PayloadOmitted marks a summary row whose payload was left out; GetBySpan serves it inlined into Data.
	PayloadOmitted bool `bun:"payload_omitted,scanonly" json:"payload_omitted,omitempty"`
	// Attachments are the image attachments the span's input references; URL is filled by the handler.
	Attachments []EntryAttachment `bun:"-" json:"attachments,omitempty"`
}

// TraceBlob is one payload element of a session's spans — an input item, a
// tool's result, the system prompt — stored once per session and referenced
// by hash from TraceEvent.Refs. It lives and dies with the session's trace.
type TraceBlob struct {
	bun.BaseModel `bun:"table:trace_blobs,alias:tb"`

	SessionID string `bun:"session_id,pk,type:uuid"`
	Hash      []byte `bun:"hash,pk"`
	// Body is the element's JSON, gzip-compressed when that made it smaller.
	Body []byte `bun:"body,notnull"`
}

// Sandbox is a complete sandbox definition, where it runs and what runs on
// it — see decisions §5.36.
type Sandbox struct {
	bun.BaseModel `bun:"table:sandboxes,alias:sb"`

	ID   string `bun:"id,pk,type:uuid" json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	// Type is the backend, one of SandboxTypes; frozen while projects live on the sandbox.
	Type string `bun:"type,notnull" json:"type"`

	// Config holds the settings as JSON (DockerConfig or E2BConfig), exchanged with the API as a raw object.
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	// Prompt is appended to the instructions of every agent in a session bound to a project here; an edit reaches the next run.
	Prompt string `bun:"prompt" json:"prompt,omitempty"`

	// Revision counts the row's writes, name-only included; every update carries the one it expects.
	Revision int64 `bun:"revision,notnull,default:1" json:"revision,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	// Supports is the type's capability row (SandboxSupports), derived per response and never stored.
	Supports SandboxSupports `bun:"-" json:"supports"`
}

// DockerConfig is the Sandbox.Config payload for type "docker": the daemon
// and how to reach it (frozen while projects live on the sandbox), then content.
type DockerConfig struct {
	// Host reaches a remote daemon, "ssh://user@host[:port]" or "tcp://host:port"; empty is the local daemon.
	Host string `json:"host,omitempty"`
	// SSH authentication for an ssh:// Host, tried in order: agent, key file, password; host keys verify against known_hosts unless insecure.
	SSHUseAgent        bool   `json:"ssh_use_agent,omitempty"`
	SSHKeyFile         string `json:"ssh_key_file,omitempty"`
	SSHPassword        string `json:"ssh_password,omitempty"` // write-only (mask semantics)
	SSHKnownHosts      string `json:"ssh_known_hosts,omitempty"`
	SSHInsecureHostKey bool   `json:"ssh_insecure_host_key,omitempty"`

	Image   string `json:"image"`
	Runtime string `json:"runtime,omitempty"` // OCI runtime (e.g. "runsc" for gVisor)
	User    string `json:"user,omitempty"`    // user[:group] the container runs as; "" = the image's own user
	// Network is the docker network the container joins; empty leaves it with none.
	Network string `json:"network,omitempty"`
	// MemoryMB and CPUs cap the container; 0 takes the workbench default (sandboxes.DefaultMemoryMB, DefaultCPUs).
	MemoryMB         int64   `json:"memory_mb,omitempty"`
	CPUs             float64 `json:"cpus,omitempty"`
	MaxReadFileBytes int64   `json:"max_read_file_bytes,omitempty"` // read_file cap in bytes; 0 = backend default (8 MiB)
}

// E2BConfig is the Sandbox.Config payload for type "e2b": the service and
// its template; APIURL, Domain, TemplateID, AutoPause and AllowInternet freeze while projects live on the sandbox.
type E2BConfig struct {
	// APIURL is the control plane base; empty means E2B's own.
	APIURL string `json:"api_url,omitempty"`
	// Domain is the suffix a sandbox's public hosts are built from; empty means E2B's own.
	Domain string `json:"domain,omitempty"`
	// APIKey authenticates the control plane. Write-only (mask semantics).
	APIKey string `json:"api_key,omitempty"`
	// DataPlaneAuth selects the in-sandbox daemon's credential: "" (auto), "access_token", "api_key" or "none".
	DataPlaneAuth string `json:"data_plane_auth,omitempty"`

	// TemplateID names a template that already exists on the service.
	TemplateID string `json:"template_id"`
	// User is the account commands run as, one the template provides; "" is e2b's default ("user").
	User string `json:"user,omitempty"`
	// TimeoutSeconds is the lease a sandbox is created and refreshed with; 0 uses the backend default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// AutoPause pauses rather than kills on lease expiry; true when absent, serialized without omitempty so false is kept.
	AutoPause bool `json:"auto_pause"`
	// AllowInternet gives the sandbox outbound network access.
	AllowInternet bool `json:"allow_internet,omitempty"`
	// MaxReadFileBytes caps read_file; 0 = the backend default (8 MiB).
	MaxReadFileBytes int64 `json:"max_read_file_bytes,omitempty"`
}

// Project is one user's working tree on one sandbox (decisions §5.28):
// the unit a session binds, stored in the named volume the project's
// container mounts at /workspace. The storage name is derived from the id,
// never stored.
type Project struct {
	bun.BaseModel `bun:"table:projects,alias:pj"`

	ID      string `bun:"id,pk,type:uuid"               json:"id"`
	OwnerID string `bun:"owner_id,notnull,type:uuid"    json:"owner_id"`
	// SandboxID is what the project runs on; it may move only to a sandbox of the same type and destination (409 otherwise).
	SandboxID string `bun:"sandbox_id,notnull,type:uuid" json:"sandbox_id"`
	// Name is display only, unique per (owner, sandbox); a rename moves nothing.
	Name string `bun:"name,notnull"                json:"name"`
	// Env is the canonical environment the container is created with, sealed at rest; GET /projects/{id} alone returns it, values masked.
	Env string `bun:"env,type:text,nullzero" json:"-"`
	// InstanceRef is the backend's handle on the live instance, for a backend that does not derive it from the project id.
	InstanceRef string `bun:"instance_ref,nullzero" json:"-"`
	// Revision is the expected-revision CAS every update lands against.
	// RuntimeGen moves when the project's content or its sandbox changes; the instance cache and terminals fence on it.
	Revision   int64     `bun:"revision,notnull,default:1"    json:"revision,omitempty"`
	RuntimeGen int64     `bun:"runtime_gen,notnull,default:1" json:"-"`
	CreatedAt  time.Time `bun:"created_at,notnull"            json:"created_at"`
	UpdatedAt  time.Time `bun:"updated_at,notnull"            json:"updated_at"`
	// StorageHint names where the files live (the named volume), derived per response for admins only and never stored.
	StorageHint string `bun:"-" json:"storage_hint,omitempty"`
	// SessionCount is how many sessions bind this project, filled by List.
	SessionCount int `bun:"session_count,scanonly" json:"session_count,omitempty"`
}

// Guardrail is a stored guardrail definition. Mode selects the check logic:
// "regex" uses Config.Pattern; "max_length" uses Config.MaxLength.
type Guardrail struct {
	bun.BaseModel `bun:"table:guardrails,alias:gr"`

	ID          string `bun:"id,pk,type:uuid"    json:"id"`
	Name        string `bun:"name,notnull"       json:"name"`
	Description string `bun:"description"        json:"description"`
	// Stages are the run stages this guardrail inspects: input, output, tool_input, tool_output.
	Stages []string        `bun:"stages,type:text"   json:"stages"`
	Mode   string          `bun:"mode,notnull"       json:"mode"` // regex | max_length
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`
	// Blocking, at the input stage, runs the guardrail before the first model call as a gate; no effect at other stages.
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

	RunID     string `bun:"run_id,pk,type:uuid"    json:"run_id"`
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	// Kind is "" for a tool call the run paused on, ApprovalKindStep for a workflow step waiting to start.
	Kind          string `bun:"kind"                   json:"kind,omitempty"`
	AgentConfigID string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	ProjectID     string `bun:"project_id,nullzero,type:uuid" json:"project_id,omitempty"`
	// State is the JSON from agents.RunState.MarshalJSON. Hidden from the API.
	State string `bun:"state,type:text,notnull" json:"-"`
	// ToolCalls is the JSON array of pending tool calls ([]PendingToolCall) shown to the user.
	ToolCalls json.RawMessage `bun:"tool_calls,type:text,nullzero" json:"tool_calls,omitempty"`
	// UserInput is the text of the message that started the paused turn; the UI shows it while the run waits.
	UserInput string    `bun:"user_input,type:text,nullzero" json:"user_input,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"            json:"created_at"`
}

// ApprovalKindStep marks a pending approval that gates a workflow step's start
// (WorkflowStep.PauseBefore); its one tool call is named StepApprovalToolName.
const (
	ApprovalKindStep     = "step"
	StepApprovalToolName = "start_step"
)

// PendingToolCall is one tool call awaiting approval, projected from a run's
// interruptions for the approvals listing.
type PendingToolCall struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Arguments  string `json:"arguments"`
}

// ParsedToolCalls decodes the ToolCalls JSON, returning nil on malformed data.
func (p *PendingApproval) ParsedToolCalls() []PendingToolCall {
	if len(p.ToolCalls) == 0 {
		return nil
	}
	var out []PendingToolCall
	if err := json.Unmarshal(p.ToolCalls, &out); err != nil {
		return nil
	}
	return out
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *Guardrail) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id, timestamps and scope; bun invokes it on insert and update.
func (m *AgentConfig) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if err := stampScope(q, &m.Scope, m.OwnerID); err != nil {
		return err
	}
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id, timestamps and scope; bun invokes it on insert and update.
func (m *McpServerConfig) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if err := stampScope(q, &m.Scope, m.OwnerID); err != nil {
		return err
	}
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// stampScope pins the scope/owner invariant on INSERT: an unstamped write lands
// private, and every row records its creator — see decisions §5.29.
func stampScope(q bun.Query, scope *string, ownerID string) error {
	if _, ok := q.(*bun.InsertQuery); !ok {
		return nil
	}
	*scope = NormalizeScope(*scope)
	if ownerID == "" {
		return fmt.Errorf("a scoped row needs an owner")
	}
	return nil
}

// BeforeAppendModel stamps the id, timestamps and scope; bun invokes it on insert and update.
func (m *Skill) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if err := stampScope(q, &m.Scope, m.OwnerID); err != nil {
		return err
	}
	// The label is derived, never supplied: it is what the unique name indexes
	// key on, so it must follow SourceRepo on every write (decisions §5.31).
	m.RepoLabel = repoLabelOf(m.SourceRepo)
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *Memory) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id, timestamps and scope; bun invokes it on insert and update.
func (m *Workflow) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if err := stampScope(q, &m.Scope, m.OwnerID); err != nil {
		return err
	}
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id, timestamps and scope; bun invokes it on insert and update.
func (m *Provider) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if err := stampScope(q, &m.Scope, m.OwnerID); err != nil {
		return err
	}
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *Sandbox) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *Project) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// User is an account: the one implicit local user (LocalUserID) in --auth
// token mode, a row per first login in --auth oauth mode — so ownership has
// a referent in both.
type User struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	ID    string `bun:"id,pk,type:uuid" json:"id"`
	Email string `bun:"email,notnull"  json:"email"` // lowercased; unique via idx_users_email
	Name  string `bun:"name,nullzero"  json:"name,omitempty"`
	// AvatarURL is the provider's picture URL; the CSP admits the configured providers' image hosts.
	AvatarURL string `bun:"avatar_url,nullzero" json:"avatar_url,omitempty"`
	Role      string `bun:"role,notnull"        json:"role"` // RoleAdmin | RoleMember
	// DisabledAt is when an admin switched the account off; no credential authenticates until it is cleared.
	DisabledAt time.Time `bun:"disabled_at,nullzero" json:"disabled_at,omitzero"`

	LastLoginAt time.Time `bun:"last_login_at,nullzero" json:"last_login_at,omitzero"`
	CreatedAt   time.Time `bun:"created_at,notnull"     json:"created_at"`
	UpdatedAt   time.Time `bun:"updated_at,notnull"     json:"updated_at"`
}

// Identity links one OAuth login (provider + subject) to a user. A user may
// hold several — logins with the same verified email merge into one account.
type Identity struct {
	bun.BaseModel `bun:"table:identities,alias:idn"`

	ID       string `bun:"id,pk,type:uuid"   json:"id"`
	UserID   string `bun:"user_id,notnull,type:uuid" json:"user_id"`
	Provider string `bun:"provider,notnull"  json:"provider"`
	Subject  string `bun:"subject,notnull"   json:"subject"` // unique with provider via idx_identities_subject

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// AuthToken is a credential row: a browser session or a personal access token.
// Only the SHA-256 of the secret is stored; the plaintext exists exactly once,
// in the response that created it.
type AuthToken struct {
	bun.BaseModel `bun:"table:auth_tokens,alias:at"`

	ID        string `bun:"id,pk,type:uuid"    json:"id"`
	UserID    string `bun:"user_id,notnull,type:uuid" json:"user_id"`
	Kind      string `bun:"kind,notnull"       json:"kind"` // TokenKindSession | TokenKindPAT
	TokenHash string `bun:"token_hash,notnull" json:"-"`    // unique via idx_auth_tokens_hash
	Name      string `bun:"name,nullzero"      json:"name,omitempty"`

	LastUsedAt time.Time `bun:"last_used_at,nullzero" json:"last_used_at,omitzero"`
	ExpiresAt  time.Time `bun:"expires_at,nullzero"   json:"expires_at,omitzero"` // zero on a PAT = never
	CreatedAt  time.Time `bun:"created_at,notnull"    json:"created_at"`
	UpdatedAt  time.Time `bun:"updated_at,notnull"    json:"updated_at"`
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *User) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *Identity) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *AuthToken) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// BeforeAppendModel mints the id on insert; bun invokes it on insert and update.
func (m *TraceEvent) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if _, ok := q.(*bun.InsertQuery); ok && m.ID == "" {
		m.ID = NewTimeID() // append-heavy table: time-ordered ids (see NewTimeID)
	}
	return nil
}
