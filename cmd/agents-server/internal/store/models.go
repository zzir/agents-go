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
	// Gen names which generation of this id owns the session's entries; see
	// session.Ref.
	Gen string `bun:"gen,notnull"          json:"-"`
	// OwnerID is the user the conversation belongs to — the only ownership
	// column: a task's hidden session inherits it from its parent, a trigger
	// fires into a session, an approval is filed on one. Content is the
	// owner's alone; an admin may list, stop and delete (workbench invariant 42).
	OwnerID string `bun:"owner_id,notnull,type:uuid" json:"owner_id"`
	Name    string `bun:"name,notnull"         json:"name"`
	Pinned  bool   `bun:"pinned"               json:"pinned"`
	// Hidden marks a session that exists to serve another one — a background
	// task's transcript. Listings leave it out by default.
	Hidden        bool   `bun:"hidden"               json:"hidden,omitempty"`
	AgentConfigID string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	// ProjectID is the session's PERMANENT binding: the first project-carrying
	// run CAS-writes it (BindProjectIfEmpty) and it is never rewritten —
	// decisions §5.28. The project pins the target, so binding the project
	// binds the machine too.
	ProjectID string `bun:"project_id,nullzero,type:uuid" json:"project_id,omitempty"`
	// Planning is the session's plan phase: true means its next run starts
	// read-only until a plan is approved. Set by the person, cleared by the
	// approved submit_plan, copied by a fork — workbench invariant 33.
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
	// RunID is the id of the task's CURRENT run: a retry replaces it, and so
	// does each step of a workflow. It is distinct from the task id because the
	// task is the durable entity and a run is one try at it — which is what
	// makes a retry expressible at all.
	RunID string `bun:"run_id,nullzero,type:uuid" json:"run_id,omitempty"`
	// Kind is the SDK's host-defined discriminator: "" for a sub-agent task,
	// TaskKindWorkflow for a workflow execution.
	Kind string `bun:"kind" json:"kind,omitempty"`
	// State is the SDK's opaque per-job record — for a workflow, the encoded
	// WorkflowState (the definition snapshot and where the sequence stands).
	State           json.RawMessage `bun:"state,type:text,nullzero" json:"state,omitempty"`
	ParentSessionID string          `bun:"parent_session_id,notnull,type:uuid" json:"parent_session_id"`
	// ParentSessionGen and ChildSessionGen are the GENERATIONS of the sessions
	// this row names (session.Ref): a row matched on the id alone would attach
	// itself to a replacement created under the same name (spec §2.13). Bound
	// at insert, compared on every by-session read (liveParent / liveChild).
	ParentSessionGen string `bun:"parent_session_gen" json:"-"`
	ParentRunID      string `bun:"parent_run_id,nullzero,type:uuid" json:"parent_run_id,omitempty"`
	ToolCallID       string `bun:"tool_call_id"         json:"tool_call_id,omitempty"`
	Label            string `bun:"label"                json:"label,omitempty"`
	AgentConfigID    string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	ChildSessionID   string `bun:"child_session_id,notnull,type:uuid" json:"child_session_id"`
	ChildSessionGen  string `bun:"child_session_gen"        json:"-"`
	// Depth is how many task hops from a user-initiated run. Persisted because
	// it is the ONLY input to the recursion bound: dropping it made every task
	// report depth 0, so MaxDepth — the documented backstop against a task
	// spawning tasks forever — could never trip on this host.
	Depth int `bun:"depth" json:"depth,omitempty"`
	// Attempt counts this task's runs: 1 for the original, one more per retry.
	// Zero reads as the first attempt (the SDK's AttemptNo contract).
	Attempt int `bun:"attempt" json:"attempt,omitempty"`
	// ParentAgentConfigID / ParentProjectID snapshot the spawning run's
	// configuration so the completion notification (and a retry) can start a
	// run with the same setup.
	ParentAgentConfigID string `bun:"parent_agent_config_id,nullzero,type:uuid" json:"-"`
	ParentProjectID     string `bun:"parent_project_id,nullzero,type:uuid" json:"-"`
	Status              string `bun:"status,notnull"     json:"status"`
	Summary             string `bun:"summary,nullzero"   json:"summary,omitempty"`
	// Result is the task's full final output. The row summary (and the wake
	// notification) stay truncated to keep prompts and lists lean; the parent
	// model pulls this on demand through task_status.
	Result string `bun:"result,nullzero" json:"-"`
	// Dismissed hides a terminal task from the conversation's live strip; the
	// panel still lists it. A retry clears it — the work is live again.
	Dismissed bool `bun:"dismissed" json:"dismissed,omitempty"`
	// MaxAttempts is the ceiling Attempt is measured against — filled for the
	// wire, not stored: the task manager's configuration, not a fact about the
	// task. Clients derive "retryable" from it and the status they track.
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
	// Description is what this agent is FOR, in a sentence — the text an
	// automatic agent picker will match a request against (not the model-facing
	// instructions).
	Description string `bun:"description,nullzero" json:"description,omitempty"`
	// Avatar is the agent's picture as a same-origin path into the built-in
	// catalog ("/avatars/<name>.svg"); empty renders an initial. Handlers
	// reject anything else — external URLs would be blocked by CSP anyway.
	Avatar       string `bun:"avatar,nullzero"      json:"avatar,omitempty"`
	Instructions string `bun:"instructions"   json:"instructions"`
	Model        string `bun:"model"          json:"model"`
	// ProviderID names the Provider row this agent reaches its model through —
	// a column, so referential integrity is expressible in SQL. Empty reaches
	// no credential: the run fails its pre-flight (decisions §5.30).
	ProviderID string `bun:"provider_id,nullzero,type:uuid" json:"provider_id,omitempty"`
	// ContextWindow is the model's window in tokens, declared rather than
	// discovered — no provider reports it on a response. It sits beside Model
	// because it describes the model, not the endpoint: two agents on one
	// provider may run different models. 0 leaves the Context panel showing
	// occupancy without a denominator.
	ContextWindow int `bun:"context_window" json:"context_window,omitempty"`

	// The remaining knobs are grouped into JSON category columns (see
	// agent_config_groups.go) so the table holds only category columns and a new
	// setting needs no schema change. In the REST API each is a nested object.
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

	// Scope/OwnerID: row visibility and its permanent creator — decisions §5.29.
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
	// Type selects the backend (bridge.ProviderType*). Empty means openai, the
	// value that predates the field.
	Type string `bun:"type"      json:"type,omitempty"`
	// AuthMode is "" (API key) or a mode the backend offers, validated against
	// the provider registry on save.
	AuthMode string `bun:"auth_mode" json:"auth_mode,omitempty"`
	// APIKey is masked on the way out (see sanitizeProvider) and restored from
	// the stored row when a client sends the mask back.
	APIKey  string `bun:"api_key"   json:"api_key,omitempty"`
	BaseURL string `bun:"base_url"  json:"base_url,omitempty"`
	// ChatGPTToken is the serialized OAuth token for auth_mode chatgpt_login.
	// It lives on the provider because it IS this endpoint's credential — every
	// agent pointed here shares the one login instead of each re-authenticating.
	// Never serialized (json:"-"); preserved across CRUD updates.
	ChatGPTToken string `bun:"chatgpt_token,type:text,nullzero" json:"-"`
	// ChatGPTLoggedIn is the API-facing derived login signal (set when
	// sanitizing); the token itself never leaves the server.
	ChatGPTLoggedIn bool `bun:"-" json:"chatgpt_logged_in,omitempty"`

	// Scope/OwnerID: row visibility and its permanent creator — decisions §5.29.
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
	// Enabled deliberately carries no bun default tag: with `default:true`,
	// bun swaps a zero-value false for SQL DEFAULT on insert, silently
	// enabling a server that was created with enabled=false.
	Enabled bool `bun:"enabled,notnull"        json:"enabled"`

	// Config holds the connection settings as JSON (HTTPMcpConfig — the
	// streamable_http transport is the only one the server speaks, decisions §5.25).
	// Stored as TEXT and exchanged with the API as a raw JSON object.
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	// OAuthToken is the JSON-serialized OAuth grant obtained during the OAuth
	// flow: the oauth2.Token plus the token endpoint and client credentials
	// needed to refresh it across restarts (bridge.tokenPayload). Stored
	// separately from Config so that regular CRUD updates (which overwrite
	// Config) don't erase it, and hidden from the API (json:"-").
	OAuthToken string `bun:"oauth_token,type:text,nullzero" json:"-"`

	// Scope/OwnerID: row visibility and its permanent creator — decisions §5.29.
	Scope   string `bun:"scope,notnull"                 json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid"   json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull"     json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull"     json:"updated_at"`
}

// McpRetryConfig holds the per-request retry settings, embedded in
// HTTPMcpConfig. A single transient failure on list_tools/call_tool
// otherwise aborts the whole run.
type McpRetryConfig struct {
	// MaxRetryAttempts retries a failed list_tools/call_tool this many times.
	// 0 (default) disables retries; -1 retries indefinitely.
	MaxRetryAttempts int `json:"max_retry_attempts,omitempty"`
	// RetryBackoffMs is the base delay (milliseconds) for exponential backoff
	// between retries. 0 leaves the SDK default (1s) when retries are enabled.
	RetryBackoffMs int `json:"retry_backoff_ms,omitempty"`
}

// HTTPMcpConfig is the McpServerConfig.Config payload (streamable HTTP).
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
	// UseStructuredContent uses a tool result's structuredContent field
	// exclusively (default: use the content blocks). For servers that only
	// populate the structured field.
	UseStructuredContent bool `json:"use_structured_content,omitempty"`
}

// Skill is one stored SKILL.md document (decisions §5.26). Name and Description
// are denormalized from the content's frontmatter at save time — the content
// is the document, the columns are its index entry.
type Skill struct {
	bun.BaseModel `bun:"table:skills,alias:sk"`

	ID          string `bun:"id,pk,type:uuid" json:"id"`
	Name        string `bun:"name,notnull"    json:"name"` // unique per scope (decisions §5.29)
	Description string `bun:"description,notnull" json:"description"`
	// Content is the full SKILL.md; capped at write time (maxSkillBytes) and
	// omitted from list responses (ListMeta).
	Content string `bun:"content,notnull,type:text" json:"content,omitempty"`

	// Source records where an imported skill came from — the repo or raw URL,
	// the path inside the repo, and the commit it was fetched at — so a
	// re-import can match and refresh it. All empty for a skill authored in
	// the workbench.
	SourceRepo string `bun:"source_repo,nullzero" json:"source_repo,omitempty"`
	SourcePath string `bun:"source_path,nullzero" json:"source_path,omitempty"`
	SourceSHA  string `bun:"source_sha,nullzero"  json:"source_sha,omitempty"`
	// RepoLabel is SourceRepo reduced to the model-facing prefix ("owner/repo",
	// or the host), materialized in BeforeAppendModel because the unique name
	// indexes key on it (decisions §5.31).
	RepoLabel string `bun:"repo_label,nullzero" json:"repo_label,omitempty"`
	// Detached marks an imported skill edited in the workbench: a re-import
	// skips it instead of overwriting the local edit.
	Detached bool `bun:"detached,notnull" json:"detached,omitempty"`

	// Scope/OwnerID: row visibility and its permanent creator — decisions §5.29.
	Scope   string `bun:"scope,notnull"                 json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid"   json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// Memory is a stored key/content fact, either global or scoped to an agent config.
type Memory struct {
	bun.BaseModel `bun:"table:memories,alias:mem"`

	ID            string    `bun:"id,pk,type:uuid"     json:"id"`
	AgentConfigID string    `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	Key           string    `bun:"key,notnull"          json:"key"`
	Content       string    `bun:"content,notnull"      json:"content"`
	Metadata      string    `bun:"metadata"             json:"metadata,omitempty"`
	CreatedAt     time.Time `bun:"created_at,notnull"   json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"   json:"updated_at"`
}

// Attachment is one uploaded image: metadata only — the bytes live in the
// configured S3-compatible bucket under Key, and session entries reference
// the row by id (an "agents-attachment:<id>" sentinel URL) that is resolved
// to the bucket's public URL only at the model boundary.
type Attachment struct {
	bun.BaseModel `bun:"table:attachments,alias:att"`

	ID      string `bun:"id,pk,type:uuid"           json:"id"`
	OwnerID string `bun:"owner_id,notnull,type:uuid" json:"owner_id"`
	// Key addresses the object in the bucket; the public URL is derived from
	// the CURRENT s3_public_base_url setting, so moving buckets means moving
	// objects, never rewriting history.
	Key  string `bun:"key,notnull"  json:"key"`
	Mime string `bun:"mime,notnull" json:"mime"`
	Size int64  `bun:"size,notnull" json:"size"`
	// Bound flips when a run accepts the attachment; an unbound row past the
	// grace window is an orphan the reaper collects, object included.
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
	SkillsIndexChars   int `json:"skills_index_chars,omitempty"`
	// Tools are the locally attached tools, bucketed by what attached them.
	// MCP is absent here: its tools live on the server, not on the agent, and
	// are sized by the read path (which is also the only place a live server
	// can be asked).
	Tools []ToolBucket `json:"tools,omitempty"`
	// MCPServerIDs are the servers the build wired up, in config order.
	MCPServerIDs []string `json:"mcp_server_ids,omitempty"`
}

// ToolBucket is one origin's share of the tool surface.
type ToolBucket struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
	Chars  int    `json:"chars"`
	// Unavailable marks a bucket that could not be measured (an MCP server that
	// is disconnected or did not answer). Reported as unknown, never as zero.
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
	// ParentRunID is the run's LINEAGE: for a task wake-up run, the run whose
	// spawn started the chain. Recorded on the trace itself so the panel's run
	// grouping reads it directly — deriving it from task rows or notification
	// text broke on every surface that does not carry them (forks above all).
	ParentRunID string `bun:"parent_run_id,nullzero,type:uuid" json:"parent_run_id,omitempty"`
	Kind        string `bun:"kind,notnull"         json:"kind"`
	SpanID      string `bun:"span_id"              json:"span_id,omitempty"`
	ParentID    string `bun:"parent_id"            json:"parent_id,omitempty"`
	Name        string `bun:"name,notnull"         json:"name"`
	Detail      string `bun:"detail"               json:"detail,omitempty"`
	Error       string `bun:"error"                json:"error,omitempty"`
	// Data is the span's metadata JSON. Its payload fields live in trace_blobs
	// (decisions §5.50): Layout names each one and its element count, Refs is
	// the sha256 of every element in that order, 32 bytes each. Both NULL
	// when the span has no payload.
	Data      string    `bun:"data"                 json:"data,omitempty"`
	Layout    string    `bun:"layout,nullzero"      json:"-"`
	Refs      []byte    `bun:"refs,nullzero"        json:"-"`
	StartedAt string    `bun:"started_at"           json:"started_at,omitempty"`
	EndedAt   string    `bun:"ended_at"             json:"ended_at,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull"   json:"created_at"`
	// PayloadOmitted marks a summary row (TraceStore.ListSummaryBySession)
	// whose payload was left out; GetBySpan serves it inlined into Data.
	PayloadOmitted bool `bun:"payload_omitted,scanonly" json:"payload_omitted,omitempty"`
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

// Sandbox is a complete sandbox definition: WHERE it runs and WHAT runs on
// it, one row (decisions §5.36). Its fields split by MUTABILITY: the type and
// the destination are a project's identity and freeze while projects live on
// the sandbox (SandboxIdentityChanged); everything else is content, reaching
// bound sessions at their next run (workbench invariant 45).
type Sandbox struct {
	bun.BaseModel `bun:"table:sandboxes,alias:sb"`

	ID   string `bun:"id,pk,type:uuid" json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	// Type is the backend — one of SandboxTypes.
	Type string `bun:"type,notnull" json:"type"`

	// Config holds the settings as JSON (DockerConfig or E2BConfig). Stored
	// as TEXT and sent to/received from the API as a raw JSON object (no
	// double-encoding).
	Config json.RawMessage `bun:"config,type:text,nullzero" json:"config,omitempty"`

	// Prompt is appended to the instructions of every agent in a session bound
	// to a project on this sandbox — content, not identity, and not a
	// retirement trigger: an edit reaches the next run without replacing live
	// instances.
	Prompt string `bun:"prompt" json:"prompt,omitempty"`

	// Revision counts this row's WRITES, name-only included — the
	// expected-revision CAS every update carries. No runtime generation here:
	// the ONE runtime axis is the project's (decisions §5.33), bumped on every
	// project naming this sandbox when its content changes.
	Revision int64 `bun:"revision,notnull,default:1" json:"revision,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`

	// Supports is the type's capability row, derived per response (never
	// stored) — see SandboxSupports.
	Supports SandboxSupports `bun:"-" json:"supports"`
}

// DockerConfig is the Sandbox.Config payload for type "docker". The first
// group is the DESTINATION — which daemon, and how to reach it — and freezes
// while projects live on the sandbox; the rest is content.
type DockerConfig struct {
	// Host reaches a remote daemon: "ssh://user@host[:port]" (pure-Go SSH to
	// the remote's docker socket) or "tcp://host:port". Empty = the local
	// daemon.
	Host string `json:"host,omitempty"`
	// The SSH authentication for an ssh:// Host: methods are tried in order
	// (agent, key file, password); host keys verify against known_hosts
	// unless the insecure flag opts out.
	SSHUseAgent        bool   `json:"ssh_use_agent,omitempty"`
	SSHKeyFile         string `json:"ssh_key_file,omitempty"`
	SSHPassword        string `json:"ssh_password,omitempty"` // write-only (mask semantics)
	SSHKnownHosts      string `json:"ssh_known_hosts,omitempty"`
	SSHInsecureHostKey bool   `json:"ssh_insecure_host_key,omitempty"`

	Image   string `json:"image"`
	Runtime string `json:"runtime,omitempty"` // OCI runtime (e.g. "runsc" for gVisor)
	User    string `json:"user,omitempty"`    // user[:group] the container runs as; "" = the image's own user
	// Network names the docker network the container joins; empty leaves it
	// with no network at all.
	Network string `json:"network,omitempty"`
	// MemoryMB / CPUs cap the container's resources; 0 = unlimited (memory)
	// and the daemon default (cpus).
	MemoryMB         int64   `json:"memory_mb,omitempty"`
	CPUs             float64 `json:"cpus,omitempty"`
	MaxReadFileBytes int64   `json:"max_read_file_bytes,omitempty"` // read_file cap in bytes; 0 = backend default (8 MiB)
}

// E2BConfig is the Sandbox.Config payload for type "e2b": which service, and
// which of its templates. APIURL and Domain are the destination; they, the
// template and the lifecycle policy a /connect resume cannot re-apply
// (template_id, auto_pause, allow_internet) all freeze while projects live on
// the sandbox (see SandboxIdentityChanged).
type E2BConfig struct {
	// APIURL is the control plane base; empty means E2B's own.
	APIURL string `json:"api_url,omitempty"`
	// Domain is the suffix a sandbox's public hosts are built from; empty
	// means E2B's own.
	Domain string `json:"domain,omitempty"`
	// APIKey authenticates the control plane. Write-only (mask semantics).
	APIKey string `json:"api_key,omitempty"`
	// DataPlaneAuth selects the credential the in-sandbox daemon takes:
	// "" (auto), "access_token", "api_key" or "none". Configuration rather
	// than a constant because the compatible services differ.
	DataPlaneAuth string `json:"data_plane_auth,omitempty"`

	// TemplateID names a template that already exists on the service.
	TemplateID string `json:"template_id"`
	// User is the account commands run as; "" = e2b's default ("user"). It must
	// be an account the template provides, so a template that names its account
	// differently is reachable. Passed per request, so editing it needs no rebuild.
	User string `json:"user,omitempty"`
	// TimeoutSeconds is the lease a sandbox is created and refreshed with;
	// 0 uses the backend default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// AutoPause makes the lease PAUSE the sandbox rather than kill it, so an
	// idle project keeps its files. Defaults to true when absent (set by
	// NormalizeSandboxConfig); serialized explicitly — no omitempty — so a
	// false (kill on expiry) is never confused with an unset field.
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
	// SandboxID is what the project runs on — the machine and the image, set
	// at creation and never writable afterwards. The image half IS editable,
	// on the sandbox row: the freeze is on which row, not on its content
	// (decisions §5.36).
	SandboxID string `bun:"sandbox_id,notnull,type:uuid" json:"sandbox_id"`
	// Name is display only — the storage is keyed by ID, so a rename moves
	// nothing. Unique per (owner, sandbox) via idx_projects_owner_sandbox_name.
	Name string `bun:"name,notnull"                json:"name"`
	// Env is the canonical environment the container is created with
	// (NormalizeProjectEnv), values sealed at rest. json:"-" is the default
	// that keeps it off every listing: GET /projects/{id} is the one endpoint
	// that returns it, and it returns names with masked values.
	Env string `bun:"env,type:text,nullzero" json:"-"`
	// InstanceRef is the backend's own handle on the project's live sandbox,
	// for a backend whose instance id it does not derive from the project id.
	// Docker derives its container name and needs none; it exists so a remote
	// backend has somewhere to keep the id its API minted.
	InstanceRef string `bun:"instance_ref,nullzero" json:"-"`
	// Revision is the expected-revision CAS every update lands against.
	// RuntimeGen is the workbench's ONE runtime axis: it moves when this
	// project's own content changes AND when the sandbox it names changes
	// underneath it, so the instance cache and the terminal registry
	// need a single fence rather than one per entity (decisions §5.33). A
	// rename moves neither container nor terminal.
	Revision   int64     `bun:"revision,notnull,default:1"    json:"revision,omitempty"`
	RuntimeGen int64     `bun:"runtime_gen,notnull,default:1" json:"-"`
	CreatedAt  time.Time `bun:"created_at,notnull"            json:"created_at"`
	UpdatedAt  time.Time `bun:"updated_at,notnull"            json:"updated_at"`
	// StorageHint names where the files live — the named volume on the
	// sandbox's daemon. Derived per response by the handler for admins only,
	// never stored: a delete DESTROYS that storage (decisions §5.33), so the UI
	// can say what will be lost.
	StorageHint string `bun:"-" json:"storage_hint,omitempty"`
	// SessionCount is how many sessions bind this project — filled by List
	// (scanonly), so a delete knows whether it will be refused.
	SessionCount int `bun:"session_count,scanonly" json:"session_count,omitempty"`
}

// Guardrail is a stored guardrail definition. Mode selects the check logic:
// "regex" uses Config.Pattern; "max_length" uses Config.MaxLength.
type Guardrail struct {
	bun.BaseModel `bun:"table:guardrails,alias:gr"`

	ID          string `bun:"id,pk,type:uuid"    json:"id"`
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

	RunID     string `bun:"run_id,pk,type:uuid"    json:"run_id"`
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	// Kind is what the decision is about: "" a tool call the run paused on
	// (State is the run to resume), ApprovalKindStep a workflow step waiting to
	// start (no run exists yet — approving launches it, rejecting cancels the
	// execution).
	Kind          string `bun:"kind"                   json:"kind,omitempty"`
	AgentConfigID string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	ProjectID     string `bun:"project_id,nullzero,type:uuid" json:"project_id,omitempty"`
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

// stampScope pins the scope/owner invariant on INSERT: an unstamped direct
// write lands private, and every row records its creator — see NormalizeScope
// and decisions §5.29.
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
	// AvatarURL is the provider's picture URL, carried by /auth/me and loaded
	// by the browser; the CSP admits the configured providers' image hosts.
	AvatarURL string `bun:"avatar_url,nullzero" json:"avatar_url,omitempty"`
	Role      string `bun:"role,notnull"        json:"role"` // RoleAdmin | RoleMember
	// DisabledAt, when set, is when an admin switched the account off: no
	// credential of theirs authenticates until it is cleared.
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
