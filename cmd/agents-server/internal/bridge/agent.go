// Package bridge adapts stored configuration into live agents-SDK constructs (agents, model providers, MCP servers, sandboxes, guardrails) and drives streamed runs over the WebSocket.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bravesearch"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// AgentDeps holds all the dependencies needed to build a fully configured agent.
type AgentDeps struct {
	AgentConfigs     *store.AgentConfigStore
	Providers        *store.ProviderStore
	McpServers       *store.McpServerStore
	SandboxConfigs   *store.SandboxStore
	Memories         *store.MemoryStore
	Settings         *settings.Reader
	ProviderRoutes   *store.ProviderRouteStore
	Sessions         *store.SessionStore
	Traces           *store.TraceStore
	Guardrails       *GuardrailResolver
	McpManager       *McpManager
	SandboxManager   *SandboxManager
	ChatGPTOAuth     *ChatGPTOAuth
	PendingApprovals *store.PendingApprovalStore
	Tasks            *store.TaskStore
	ContextProfiles  *store.ContextProfileStore
	Workflows        *store.WorkflowStore
	Wakeups          *store.WakeupStore
	// Audit records the acts a run performs on shared configuration (a
	// save_workflow), attributed to the session's owner; nil records nothing.
	Audit server.AuditFunc
	// Users answers the run owner's role; nil withholds the tools only an
	// admin gets.
	Users     *store.UserStore
	Workspace string
	// MaxTasks overrides the per-session live background-task cap when > 0
	// (--max-tasks; 0 keeps the built-in default).
	MaxTasks int
	// TaskManager is set by NewRunner; when non-nil, chat agents get the
	// spawn_task / task_status / task_stop tools. A BACKGROUND run never gets
	// them — that is what bounds recursion.
	TaskManager *tasks.Manager
	// SpawnTool is set by NewRunner and builds the run's spawn_task — the
	// server's own, which starts a workflow when told a name — per run,
	// because the workflows on offer change as they are edited, with no
	// restart. Attached beside the manager's TaskTools; never on a background
	// run.
	SpawnTool func(ctx context.Context) *agents.Tool
	// WorkflowTools is set by NewRunner and builds the run's get_workflow /
	// save_workflow — per run, like SpawnTool, because the save tool's
	// description names the agents on offer. Attached only when the config
	// opts in (behavior.workflow_authoring); never on a background run, and
	// save_workflow only on an admin's run (README "Ownership and roles").
	WorkflowTools func(ctx context.Context, ownerID string) []*agents.Tool
}

// BuildResult contains the built agent and its resolved model provider.
type BuildResult struct {
	Agent    *agents.Agent
	Provider agents.ModelProvider

	// Behavior, Compaction and Session are the agent config's own groups,
	// carried whole rather than copied knob by knob: a new knob is one field
	// on one group struct, with no mirror field here and no copy line in the
	// builder.
	//
	// They hold the values AS STORED. A knob that needed converting was
	// converted during the build and has a "Derived:" field below — read that
	// one. Two of them keep their name and change type, so reading the group
	// copy quietly yields the unconverted value: StopAtTools (comma-separated
	// string here, []string below) and ReasoningItemIDPolicy (string here,
	// enum below). Other group fields are already spent when the build
	// returns — HandoffDescription, DisableToolChoiceReset and
	// Session.PromptID/PromptVersion live on Agent from then on.
	//
	// Compaction.Threshold is in tokens, Compaction.Window in entries — see
	// store.NewCompactionAdapter for the defaults and the sizing rule.
	Behavior   store.BehaviorGroup
	Compaction store.CompactionGroup
	Session    store.SessionGroup

	// ProviderType is the normalized backend selector this agent was built
	// for ("openai" / "anthropic"). Handoff wiring uses it to refuse a
	// keyless target that would silently inherit a different backend.
	ProviderType string
	// ErrorHandlers are the run-level recovery handlers built from the
	// top-level config's error_handlers field (zero value when unconfigured).
	ErrorHandlers agents.RunErrorHandlers

	// TraceIncludeSensitive gates whether generation spans record request and
	// response content (the global trace_include_sensitive_data setting).
	// nil = the SDK default (include). With it off, traces keep only
	// timing/usage metadata — and the trace panel's Replay has nothing to
	// seed from, by design.
	TraceIncludeSensitive *bool

	// LogSensitive gates whether the SDK's own log records carry conversation
	// content (the log_sensitive_data setting). Separate from the trace
	// switch: they go to different places, so they are different decisions.
	LogSensitive bool

	// PlanPhase is set when the agent was built in plan mode: the run starts
	// read-only and the approved submit_plan unlocks it. A resume that
	// already executed submit_plan in this run calls Unlock() so the rebuilt
	// run continues executing instead of demanding a second plan.
	PlanPhase *middleware.PlanPhase

	// ReasoningItemIDPolicy controls whether reasoning-item ids survive across
	// turns (default preserve). Derived: Behavior stores it as a string.
	ReasoningItemIDPolicy agents.ReasoningItemIDPolicy

	// StopAtTools ends the run after a turn that called any of these tools.
	// Empty means the run continues until the model stops on its own.
	// Derived: Behavior stores it as one comma-separated string.
	StopAtTools []string

	// RunGuardrails are the entry (root) agent's guardrails, lifted to the RUN
	// level so they cover the whole run — crucially, the final output regardless
	// of which agent produced it after a handoff. Handoff-target agents keep
	// their own agent-level guardrails; the root's are moved here (and cleared
	// off the root agent) to avoid double-running.
	RunGuardrails []agents.Guardrail

	// Profile is what this build put in front of the model before the
	// conversation: the instruction layers and the tool surface, sized in
	// characters. The runner persists it per session for the Context panel;
	// it describes the ENTRY agent only, since a handoff target's surface
	// applies only after a handoff has happened.
	Profile store.PromptProfile

	// releaseSandbox drops every sandbox-instance reference this build
	// acquired — the entry agent's and each handoff target's, folded into one
	// (see agentBuildCtx.releases). Nil when the build attached no sandbox.
	// Callers go through Release.
	releaseSandbox func()
}

// Release drops the build's hold on its sandbox instance. Every builder MUST
// arrange for exactly one Release once nothing uses the built agent's tools
// any more — a run's end, an approval resume's completion — or an evicted
// instance (config update/delete, last session gone) is never closed. Safe on
// a build with no sandbox and idempotent (Acquire's release is once-guarded).
func (b *BuildResult) Release() {
	if b != nil && b.releaseSandbox != nil {
		b.releaseSandbox()
	}
}

// BuildFullAgent constructs an *agents.Agent from a config ID, loading all
// associated resources: provider, MCP tools, sandbox CodeTool, memory, and
// global settings (system_prompt). agentConfigID is required. sandboxID is
// optional — when set, only that sandbox is attached; when empty, all are.
// forUserID is who the build is for, as a run's owner would be: it decides
// which tools they would get.
func BuildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, sandboxID, forUserID string) (*BuildResult, error) {
	return buildFullAgent(ctx, deps, agentConfigID, sandboxID, "", false, forUserID)
}

// BackgroundInstructions is what a run nobody is watching has to be told. The
// missing tools say what it cannot do; this says what it cannot expect.
const BackgroundInstructions = `You are running in the background. Nobody is reading this session, so there is
nobody to ask: decide and proceed rather than requesting confirmation or
permission, and when something genuinely blocks you, finish by saying what it
was. Your final message is the only account of this turn that leaves here — end
with the outcome, not with a question.`

// buildFullAgent is BuildFullAgent with the run-scoped extras: the session's
// bound workDir for the sandbox tools, BACKGROUND awareness, and the owner
// whose role gates save_workflow ("" = ungated, the full surface).
//
// A background run is one nobody is sitting in front of — a task's, a workflow
// step's. Three things follow from that single fact, which is why they share
// one flag: it gets no task tools (capping spawn depth at one — a sequence
// cannot start a sequence either, since a workflow is what spawn_task starts
// when told a name), and no plan or todo mode, because a plan review is an
// approval nobody would ever answer.
func buildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, sandboxID, workDir string, background bool, ownerID string) (*BuildResult, error) {
	if agentConfigID == "" {
		return nil, fmt.Errorf("agent_config_id is required")
	}
	bc := &agentBuildCtx{
		stack:   make(map[string]bool),
		cache:   make(map[string]*BuildResult),
		workDir: workDir,
	}
	result, err := buildAgentFromConfig(ctx, deps, agentConfigID, sandboxID, bc)
	if err == nil {
		result.TraceIncludeSensitive = deps.Settings.BoolPtr(ctx, settings.KeyTraceIncludeSensitiveData)
		result.LogSensitive = deps.Settings.Bool(ctx, settings.KeyLogSensitiveData)
	}
	if err == nil && !background && deps.TaskManager != nil {
		// The model's background surface: four verbs — spawn (the server's,
		// which starts a workflow when told a name), status, retry, stop. A
		// background run never gets them: an execution is a task, and a task
		// cannot start one — that is what bounds recursion. The session id
		// reaches the tools through the run context, not the model: otherwise
		// one conversation could spawn tasks onto another.
		mark := len(result.Agent.Tools)
		if deps.SpawnTool != nil {
			result.Agent.Tools = append(result.Agent.Tools, deps.SpawnTool(ctx))
		} else {
			result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.SpawnTool(nil))
		}
		result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.TaskTools(nil)...)
		bucketToolsSince(result.Agent, mark, store.ToolSourceTasks, &result.Profile)
	}
	// Workflow authoring is opt-in per agent and, like the task tools, a
	// chat-only surface: a background run has nobody to approve a save
	// (README invariant 39).
	if err == nil && !background && result.Behavior.WorkflowAuthoring && deps.WorkflowTools != nil {
		mark := len(result.Agent.Tools)
		// A member reads definitions (as the API lets them) but cannot write
		// one: the REST gate holds through the tool as well.
		admin := ownerIsAdmin(ctx, deps, ownerID)
		for _, tool := range deps.WorkflowTools(ctx, ownerID) {
			if tool.ReadOnly || admin {
				result.Agent.Tools = append(result.Agent.Tools, tool)
			}
		}
		bucketToolsSince(result.Agent, mark, store.ToolSourceWorkflows, &result.Profile)
	}
	if err != nil {
		// A failed build returns no result to Release, so the sandbox
		// references acquired before the failure are dropped here.
		for _, release := range bc.releases {
			release()
		}
		return nil, err
	}
	if len(bc.releases) > 0 {
		releases := bc.releases
		result.releaseSandbox = func() {
			for _, release := range releases {
				release()
			}
		}
	}
	// Told, not merely arranged for: an agent whose toolset was quietly reduced
	// still behaves like one in a conversation — it asks for confirmation and
	// stops, and a background run has nobody to answer. A SUFFIX so it lands
	// after the agent's own instructions, which may well say to ask.
	if background && result.Agent != nil {
		result.Agent.Instructions = agents.WrapInstructions(result.Agent.Instructions, "", BackgroundInstructions)
	}
	// Lift the entry agent's guardrails to the run level so they protect the
	// whole conversation — the final output is checked even after a handoff to an
	// agent that carries no guardrails of its own. Cleared off the root agent so
	// they run once (the SDK merges run-level + producing-agent guardrails).
	// Handoff targets, built recursively, keep their own agent-level guardrails.
	if result.Agent != nil {
		result.RunGuardrails = result.Agent.Guardrails
		result.Agent.Guardrails = nil
	}
	// Plan/todo rewrite the ENTRY agent, at BUILD time rather than via
	// RunOptions.Middlewares: the server resumes runs by deserializing state
	// against a registry rebuilt from this function, and a rewrite that only
	// happened inside Run would leave the rebuilt agent without
	// submit_plan/todo_write — the approved call would fail with "tool not
	// found on agent" (the same hazard buildAgentRegistry documents for
	// sandbox tools). Last, so the gates also cover the task tools appended
	// above. BACKGROUND runs are excluded: submit_plan pauses for an approval,
	// and a background run's approval lands in a session nobody is watching —
	// the sequence would wait forever on a decision nobody can see.
	if !background && result.Agent != nil {
		mark := len(result.Agent.Tools)
		// Unconditional, both of them. A todo list is a tool the model reaches
		// for when the work is worth tracking — its own judgement, like every
		// other tool. Plan mode is a RESTRAINT, so the decision is the
		// person's: Apply installs the gates and the SESSION's phase (restored
		// on every run) decides whether they bite. Building it only for a
		// planning session would also break the approval resume — the rebuild
		// happens after the unlock, and the agent would come back without the
		// submit_plan the paused state names.
		result.Agent = middleware.Todo{}.Apply(result.Agent)
		mark = bucketToolsSince(result.Agent, mark, store.ToolSourceTodo, &result.Profile)
		result.Agent, result.PlanPhase = middleware.Plan{}.Apply(result.Agent)
		bucketToolsSince(result.Agent, mark, store.ToolSourcePlan, &result.Profile)
	}
	return result, nil
}

// agentBuildCtx threads two maps through a recursive handoff build:
//   - stack is the current recursion PATH, used purely for cycle detection; a
//     config is removed on the way out, so a legitimately shared descendant
//     (a diamond: A→B→D and A→C→D) is not mistaken for a back-edge.
//   - cache holds already-built agents so a shared descendant is built once and
//     reused across paths (also avoids the redundant rebuild).
type agentBuildCtx struct {
	stack map[string]bool
	cache map[string]*BuildResult
	// workDir is the session's bound working directory, applied to every
	// sandbox toolset built for this run — handoff-target agents included, so
	// one run sees one file system context throughout.
	workDir string
	// releases collects the sandbox-instance references every build in this
	// recursion acquired (the entry agent's and each handoff target's).
	// Collected on the CONTEXT, not the per-agent results: only the top-level
	// BuildResult reaches the caller, so a release stored on a nested result
	// would be unreachable — the whole set is folded into the top result's
	// Release, and a failed build releases it before returning.
	releases []func()
}

// errHandoffCycle marks a back-edge in the handoff graph. It is recoverable —
// the offending edge is dropped and the rest of the graph builds — unlike a
// genuine build failure (bad config), which must propagate.
var errHandoffCycle = fmt.Errorf("cycle detected in handoff chain")

func buildAgentFromConfig(ctx context.Context, deps *AgentDeps, configID, sandboxID string, bc *agentBuildCtx) (*BuildResult, error) {
	if r, ok := bc.cache[configID]; ok {
		return r, nil // already built on another path (shared / diamond node)
	}
	if bc.stack[configID] {
		return nil, fmt.Errorf("%w: %s", errHandoffCycle, configID)
	}
	bc.stack[configID] = true
	defer delete(bc.stack, configID)

	result := &BuildResult{}

	ac, err := deps.AgentConfigs.Get(ctx, configID)
	if err != nil {
		return nil, fmt.Errorf("agent config %q not found — create one in Settings > Agents", configID)
	}

	agent := &agents.Agent{
		Name:                   ac.Name,
		Model:                  ac.Model,
		HandoffDescription:     ac.Behavior.HandoffDescription,
		DisableToolChoiceReset: ac.Behavior.DisableToolChoiceReset,
	}
	result.Behavior = ac.Behavior
	result.Compaction = ac.Compaction
	result.Session = ac.Session
	if ac.Behavior.ReasoningItemIDPolicy == "omit" {
		result.ReasoningItemIDPolicy = agents.ReasoningItemIDOmit
	}

	// Decode every JSON-encoded config field once, up front, so a structural
	// error fails the build loudly. DecodeAgentSpec also backs save-time
	// validation, keeping the contract in one place.
	spec, err := DecodeAgentSpec(ac)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", ac.Name, err)
	}

	if ac.Instructions != "" {
		agent.Instructions = agents.StaticInstructions(ac.Instructions)
	}
	agent.ModelSettings = spec.ModelSettings
	result.ErrorHandlers = spec.ErrorHandlers.BuildErrorHandlers()

	result.StopAtTools = splitList(ac.Behavior.StopAtTools)

	// Guardrails — a configured guardrail that can't be resolved fails the
	// build rather than running unprotected (security config must not silently
	// no-op).
	if ac.Guardrails.Guardrails != "" && deps.Guardrails != nil {
		gs, gerr := deps.Guardrails.BuildGuardrails(ctx, ac.Guardrails.Guardrails)
		if gerr != nil {
			return nil, fmt.Errorf("agent %q: %w", ac.Name, gerr)
		}
		agent.Guardrails = append(agent.Guardrails, gs...)
	}

	var approveCommands bool
	agent.ApproveTools, approveCommands = splitApproveTools(spec.ApproveTools)
	agent.OutputType = spec.OutputType

	// Stored prompt
	if ac.Session.PromptID != "" {
		agent.Prompt = agents.StaticPrompt(agents.Prompt{
			ID:      ac.Session.PromptID,
			Version: ac.Session.PromptVersion,
		})
	}

	// Provider + retry/fallback decorators. proxyClient is reused by Brave below.
	proxyClient := ProxyHTTPClient(ctx, deps.Settings)
	result.Provider, result.ProviderType, err = resolveProvider(ctx, deps, ac, spec, proxyClient)
	if err != nil {
		return nil, err
	}

	// Every layer below is also MEASURED into result.Profile as it is added —
	// what each contributes to the request is knowable here and nowhere else
	// (see store.ContextProfile).
	layerInstructions(ctx, deps, agent, ac, &result.Profile)

	// MCP servers — an id whose server is not currently connected is skipped.
	// Their tools are not measured here: they live on the server, and asking it
	// is a network call the build must not make (see the Context handler).
	result.Profile.MCPServerIDs = attachMCPServers(ctx, deps, agent, spec)

	mark := len(agent.Tools)

	// Sandbox tools — "" means none; a build failure fails the run.
	if err := attachSandboxTools(ctx, deps, bc, agent, sandboxID, approveCommands); err != nil {
		return nil, err
	}
	mark = bucketToolsSince(agent, mark, store.ToolSourceSandbox, &result.Profile)

	// Brave Search
	attachBraveSearch(ctx, deps, agent, proxyClient)
	mark = bucketToolsSince(agent, mark, store.ToolSourceBrave, &result.Profile)

	// Skills — loaded from <workspace>/skills; spec may restrict the selection.
	result.Profile.SkillsIndexChars = attachSkills(deps, agent, spec)
	bucketToolsSince(agent, mark, store.ToolSourceSkills, &result.Profile)

	// Handoffs — recursively built; a target with its own provider gets its model
	// pre-resolved so the run uses the target's backend, not the main agent's.
	if err := buildHandoffs(ctx, deps, bc, agent, result, ac, spec, sandboxID); err != nil {
		return nil, err
	}

	result.Agent = agent
	bc.cache[configID] = result
	return result, nil
}

// splitApproveTools reads the approve list: "exec_command" in it opts into
// per-command session approval (the sandbox command gate) and is kept OUT of
// the SDK's ApproveTools, which would force approval on every call.
func splitApproveTools(names []string) (approveTools []string, approveCommands bool) {
	for _, name := range names {
		if name == "exec_command" {
			approveCommands = true
			continue
		}
		approveTools = append(approveTools, name)
	}
	return approveTools, approveCommands
}

// layerInstructions wraps the agent's own instructions in the global system
// prompt and its memories, measuring each into the profile.
func layerInstructions(ctx context.Context, deps *AgentDeps, agent *agents.Agent, ac *store.AgentConfig, prof *store.PromptProfile) {
	prof.InstructionsChars = len(ac.Instructions)
	if global := deps.Settings.String(ctx, settings.KeySystemPrompt); global != "" {
		agent.Instructions = agents.WrapInstructions(agent.Instructions, global, "")
		prof.GlobalPromptChars = len(global)
	}
	memories, err := deps.Memories.ListForAgent(ctx, ac.ID)
	if err == nil && len(memories) > 0 {
		block := buildMemoryBlock(memories)
		agent.Instructions = agents.WrapInstructions(agent.Instructions, "", block)
		prof.MemoryChars = len(block)
	}
}

// buildHandoffs recursively builds each handoff target and wires it onto the
// agent. A cycle is recoverable (the edge is dropped); any other target failure
// propagates. A keyless target on a different backend is refused rather than
// letting it silently inherit this agent's provider; a target with its own
// provider gets its model pre-resolved so the run uses the target's backend.
func buildHandoffs(ctx context.Context, deps *AgentDeps, bc *agentBuildCtx, agent *agents.Agent, result *BuildResult, ac *store.AgentConfig, spec *AgentSpec, sandboxID string) error {
	for _, hID := range spec.Handoffs {
		hResult, err := buildAgentFromConfig(ctx, deps, hID, sandboxID, bc)
		if err != nil {
			if errors.Is(err, errHandoffCycle) {
				logging.Ctx(ctx).Warn("handoff cycle, skipping edge", "error", err, "handoff_id", hID)
				continue
			}
			return fmt.Errorf("agent %q handoff %q: %w", ac.Name, hID, err)
		}
		// A keyless target would resolve through the RUN's provider at handoff
		// time; a different backend would send its model name to the wrong API.
		if hResult.Provider == nil && hResult.ProviderType != result.ProviderType {
			targetKey := hResult.ProviderType + "_api_key"
			if tdef, terr := providerDefFor(hResult.ProviderType); terr == nil {
				targetKey = tdef.SettingKey
			}
			return fmt.Errorf(
				"agent %q handoff %q: target is on the %q backend but reaches no API key, so it would inherit this agent's %q provider — point the target at a provider with a key, or set the global %s",
				ac.Name, hID, hResult.ProviderType, result.ProviderType, targetKey)
		}
		if hResult.Provider != nil && hResult.Agent.Model != "" && hResult.Agent.ModelImpl == nil {
			m, merr := hResult.Provider.Model(hResult.Agent.Model)
			if merr != nil {
				return fmt.Errorf("agent %q handoff %q: resolve model: %w", ac.Name, hID, merr)
			}
			hResult.Agent.ModelImpl = m
		}
		agent.Handoffs = append(agent.Handoffs, agents.HandoffTo(hResult.Agent))
	}
	return nil
}

// attachMCPServers wires the config's selected MCP servers onto the agent,
// skipping any whose server is not currently connected. It returns the ids it
// attached — the profile records the decision actually made, not a re-derivation
// that could race a reconnect.
func attachMCPServers(ctx context.Context, deps *AgentDeps, agent *agents.Agent, spec *AgentSpec) []string {
	var attached []string
	for _, id := range spec.Tools {
		if srv := deps.McpManager.Get(id); srv != nil {
			agent.MCPServers = append(agent.MCPServers, srv)
			attached = append(attached, id)
		} else {
			logging.Ctx(ctx).Debug("MCP server not connected, skipping", "mcp_id", id)
		}
	}
	return attached
}

// attachBraveSearch adds the Brave Search tool when a brave_api_key is set; a
// build failure is logged and skipped, not fatal.
func attachBraveSearch(ctx context.Context, deps *AgentDeps, agent *agents.Agent, proxyClient *http.Client) {
	apiKey := deps.Settings.String(ctx, settings.KeyBraveAPIKey)
	if apiKey == "" {
		return
	}
	bsOpts := bravesearch.Options{APIKey: apiKey}
	if proxyClient != nil {
		bsOpts.HTTPClient = proxyClient
	}
	bsTool, err := bravesearch.New(bsOpts)
	if err != nil {
		logging.Ctx(ctx).Warn("failed to create brave_search tool", "error", err)
		return
	}
	agent.Tools = append(agent.Tools, bsTool)
}

// attachSandboxTools builds and attaches the bound sandbox's tools when one is
// selected. A build failure fails the run rather than silently downgrading — a
// bound session must not run coding prompts with no file system. The instance
// reference is recorded on bc for the build's Release.
func attachSandboxTools(ctx context.Context, deps *AgentDeps, bc *agentBuildCtx, agent *agents.Agent, sandboxID string, approveCommands bool) error {
	if sandboxID == "" {
		return nil
	}
	sbCfg, err := deps.SandboxConfigs.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", sandboxID, err)
	}
	tools, release, err := deps.SandboxManager.SandboxTools(sbCfg, bc.workDir, approveCommands)
	if err != nil {
		return fmt.Errorf("sandbox %q: building tools: %w", sbCfg.Name, err)
	}
	bc.releases = append(bc.releases, release)
	agent.Tools = append(agent.Tools, tools...)
	return nil
}

// attachSkills loads skills from <workspace>/skills and, when spec restricts the
// selection, filters to the advertised ones; the rendered index and ReadFileTool
// share the skills root so relative paths resolve. Best-effort: a load error is
// skipped, not fatal. Returns the size of the index it added to the
// instructions, which no caller can recover afterwards (it is wrapped into one
// string with every other layer).
func attachSkills(deps *AgentDeps, agent *agents.Agent, spec *AgentSpec) int {
	if deps.Workspace == "" {
		return 0
	}
	skillsDir := filepath.Join(deps.Workspace, "skills")
	loadedSkills, err := skills.LoadRecursive(skillsDir)
	if err != nil {
		return 0
	}
	if spec.SkillsSet {
		allowed := make(map[string]bool, len(spec.Skills))
		for _, p := range spec.Skills {
			allowed[p] = true
		}
		filtered := loadedSkills[:0]
		for _, sk := range loadedSkills {
			if allowed[sk.Dir] {
				filtered = append(filtered, sk)
			}
		}
		loadedSkills = filtered
	}
	if len(loadedSkills) == 0 {
		return 0
	}
	index := skills.RenderIndex(loadedSkills)
	agent.Instructions = agents.WrapInstructions(agent.Instructions, "", index)
	agent.Tools = append(agent.Tools, skills.ReadFileTool(skillsDir))
	return len(index)
}

// bucketToolsSince records the tools appended since mark under source, and
// returns the new mark. Positional rather than name-matched: the build knows
// which step added what, and a name list would have to be kept in step with
// every tool the bridge ever attaches.
func bucketToolsSince(agent *agents.Agent, mark int, source string, prof *store.PromptProfile) int {
	if len(agent.Tools) == mark {
		return mark
	}
	b := store.ToolBucket{Source: source, Count: len(agent.Tools) - mark}
	for _, t := range agent.Tools[mark:] {
		b.Chars += store.ToolChars(t)
	}
	prof.Tools = append(prof.Tools, b)
	return len(agent.Tools)
}

// staticLocalToolNames returns the fixed names of every tool the bridge
// itself can attach to an agent in buildAgentFromConfig: the sandbox tools
// (sandbox.CodeTool + sandbox.FileTools + apply_patch), the Brave Search tool,
// and the skills reader. MCP tools never appear here — they are prefixed
// "<server name>__" at connect time (see McpManager.Connect).
func staticLocalToolNames() []string {
	return []string{
		// sandbox (exec + file tools + apply_patch)
		"exec_command", "read_file", "write_file", "list_files", "apply_patch",
		// brave_api_key setting
		"brave_search",
		// skills
		"read_skill_file",
	}
}

// ValidateAgentToolNames simulates the statically knowable part of an agent's
// final tool list and reports name collisions that the SDK would otherwise
// reject only at run time (duplicate tool names are a UserError). Statically
// checkable are the bridge's own fixed tool names and the MCP servers
// referenced by toolsJSON: every server's tools get the "<name>__" prefix, so
// selecting the same server twice, or two servers that share a name, is a
// guaranteed collision. The servers' actual tool lists are only known once
// connected and cannot be validated here.
func ValidateAgentToolNames(ctx context.Context, mcpServers *store.McpServerStore, toolsJSON string) error {
	seen := map[string]bool{}
	for _, name := range staticLocalToolNames() {
		if seen[name] {
			return fmt.Errorf("duplicate built-in tool name %q", name)
		}
		seen[name] = true
	}

	if toolsJSON == "" || mcpServers == nil {
		return nil
	}
	// A malformed tools list is rejected here (and at build time) rather than
	// silently dropping every MCP tool the agent was meant to have.
	var ids []string
	if err := json.Unmarshal([]byte(toolsJSON), &ids); err != nil {
		return fmt.Errorf("tools selection is invalid: %w", err)
	}
	// Cross-server name collisions can't happen — mcp_servers.name is unique —
	// so only the same server selected twice needs catching here (that would
	// duplicate every one of its tools under the same prefix).
	seenID := map[string]bool{}
	for _, id := range ids {
		cfg, err := mcpServers.Get(ctx, id)
		if err != nil {
			continue // unknown/removed servers are skipped when the agent is built
		}
		if seenID[id] {
			return fmt.Errorf("MCP server %q is selected twice; each of its tools would appear under the same name twice", cfg.Name)
		}
		seenID[id] = true
	}
	return nil
}

func buildMemoryBlock(memories []store.Memory) string {
	var b strings.Builder
	b.WriteString("## Memories\n")
	for _, m := range memories {
		fmt.Fprintf(&b, "- **%s**: %s\n", m.Key, m.Content)
	}
	return b.String()
}

// Provider selection — validation, construction, auth modes, setting keys —
// lives in the registry (provider_registry.go); nothing here should switch on
// a provider type.

// splitList parses a comma-separated config value into trimmed, non-empty
// entries. Operators type these by hand, so stray spaces and trailing commas
// are normalized rather than turned into tool names that match nothing.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ownerIsAdmin reports whether the run's owner may write shared
// configuration. Anything that cannot say yes — no owner, no user store, a
// failed lookup — says no.
func ownerIsAdmin(ctx context.Context, deps *AgentDeps, ownerID string) bool {
	if ownerID == "" || deps.Users == nil {
		logging.Ctx(ctx).Warn("cannot resolve the run owner's role; shared-configuration tools withheld", "owner_id", ownerID)
		return false
	}
	u, err := deps.Users.ByID(ctx, ownerID)
	if err != nil {
		logging.Ctx(ctx).Warn("resolving the run owner's role", "owner_id", ownerID, "error", err)
		return false
	}
	return u.Role == store.RoleAdmin
}
