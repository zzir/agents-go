// Package bridge assembles stored configuration into live SDK agents and runs
// them: the runner, its hub, approvals, tasks, workflows and triggers. The
// connections an agent draws on live beside it — mcpservers, sandboxes,
// providers, guardrails — and know nothing of the runner.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// AgentDeps holds all the dependencies needed to build a fully configured agent.
type AgentDeps struct {
	AgentConfigs     *store.AgentConfigStore
	Providers        *store.ProviderStore
	McpServers       *store.McpServerStore
	Sandboxes        *store.SandboxStore
	Skills           *store.SkillStore
	Projects         *store.ProjectStore
	Memories         *store.MemoryStore
	Settings         *settings.Reader
	Sessions         *store.SessionStore
	Traces           *store.TraceStore
	Guardrails       *guardrails.Resolver
	McpManager       *mcpservers.Manager
	SandboxManager   *sandboxes.Manager
	ChatGPTOAuth     *providers.ChatGPTOAuth
	PendingApprovals *store.PendingApprovalStore
	Tasks            *store.TaskStore
	ContextProfiles  *store.ContextProfileStore
	Workflows        *store.WorkflowStore
	Wakeups          *store.WakeupStore
	// Audit records the acts a run performs on shared configuration (a
	// save_workflow), attributed to the session's owner; nil records nothing.
	Audit protocol.AuditFunc
	// Users answers the run owner's role; nil withholds the tools only an
	// admin gets.
	Users *store.UserStore
	// TaskManager is set by NewRunner; when non-nil, chat agents get the
	// spawn_task / task_status / task_stop tools. A BACKGROUND run never gets
	// them — that is what bounds recursion.
	TaskManager *tasks.Manager
	// SpawnTool is set by NewRunner and builds the run's spawn_task — the
	// server's own, which starts a workflow when told a name — per run,
	// because the workflows on offer change as they are edited, with no
	// restart. Attached beside the manager's TaskTools; never on a background
	// run.
	SpawnTool func(ctx context.Context, ownerID string) *agents.Tool
	// WorkflowTools is set by NewRunner and builds the run's get_workflow /
	// save_workflow — per run, like SpawnTool, because the save tool's
	// description names the agents on offer. Attached only when the config
	// opts in (behavior.workflow_authoring); never on a background run.
	// save_workflow gates per call (decisions §5.29).
	WorkflowTools func(ctx context.Context, ownerID string) []*agents.Tool
}

// BuildResult contains the built agent and its resolved model provider.
type BuildResult struct {
	Agent    *agents.Agent
	Provider agents.ModelProvider

	// AgentIDs maps each built agent's name — the entry and every handoff
	// target — to its config id, carried on the events that announce an agent
	// by name (run.agent_start, run.handoff); run.handoff resolves it to the
	// agent's avatar. Set only on the entry build.
	AgentIDs map[string]string

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
	// With it off, traces keep only timing/usage metadata — and the trace
	// panel's Replay has nothing to seed from, by design.
	TraceIncludeSensitive bool

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
func BuildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, projectID, forUserID string) (*BuildResult, error) {
	return buildFullAgent(ctx, deps, agentConfigID, projectID, false, forUserID)
}

// BackgroundInstructions is what a run nobody is watching has to be told. The
// missing tools say what it cannot do; this says what it cannot expect.
const BackgroundInstructions = `You are running in the background. Nobody is reading this session, so there is
nobody to ask: decide and proceed rather than requesting confirmation or
permission, and when something genuinely blocks you, finish by saying what it
was. Your final message is the only account of this turn that leaves here — end
with the outcome, not with a question.`

// buildFullAgent is BuildFullAgent with the run-scoped extras: the session's
// bound project for the sandbox tools, BACKGROUND awareness, and the owner
// whose role gates save_workflow ("" = ungated, the full surface).
//
// A background run is one nobody is sitting in front of — a task's, a workflow
// step's. Three things follow from that single fact, which is why they share
// one flag: it gets no task tools (capping spawn depth at one — a sequence
// cannot start a sequence either, since a workflow is what spawn_task starts
// when told a name), and no plan or todo mode, because a plan review is an
// approval nobody would ever answer.
func buildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, projectID string, background bool, ownerID string) (*BuildResult, error) {
	if agentConfigID == "" {
		return nil, fmt.Errorf("agent_config_id is required")
	}
	bc := &agentBuildCtx{
		stack:     make(map[string]bool),
		cache:     make(map[string]*BuildResult),
		projectID: projectID,
		ownerID:   ownerID,
	}
	result, err := buildAgentFromConfig(ctx, deps, agentConfigID, bc)
	if err == nil {
		result.TraceIncludeSensitive = deps.Settings.Bool(ctx, settings.KeyTraceIncludeSensitiveData)
		result.LogSensitive = deps.Settings.Bool(ctx, settings.KeyLogSensitiveData)
		result.AgentIDs = make(map[string]string, len(bc.cache))
		for id, r := range bc.cache {
			result.AgentIDs[r.Agent.Name] = id
		}
	}
	if err == nil && !background && deps.TaskManager != nil && !result.Behavior.DisableSubagents {
		// The model's background surface: four verbs — spawn (the server's,
		// which starts a workflow when told a name), status, retry, stop. A
		// background run never gets them: an execution is a task, and a task
		// cannot start one — that is what bounds recursion. An agent may also opt
		// out (behavior.disable_subagents) to shed the schema. The session id
		// reaches the tools through the run context, not the model: otherwise
		// one conversation could spawn tasks onto another.
		mark := len(result.Agent.Tools)
		if deps.SpawnTool != nil {
			result.Agent.Tools = append(result.Agent.Tools, deps.SpawnTool(ctx, ownerID))
		} else {
			result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.SpawnTool(nil))
		}
		result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.TaskTools(nil)...)
		bucketToolsSince(result.Agent, mark, store.ToolSourceTasks, &result.Profile)
	}
	// Workflow authoring is opt-in per agent and, like the task tools, a
	// chat-only surface: a background run has nobody to approve a save
	// (workbench invariant 39).
	if err == nil && !background && result.Behavior.WorkflowAuthoring && deps.WorkflowTools != nil {
		mark := len(result.Agent.Tools)
		// Every owner may save now — a member's save lands in their private
		// set; only a global workflow's edit stays the admin's (saveWorkflow
		// decides per call, mirroring the REST gate).
		result.Agent.Tools = append(result.Agent.Tools, deps.WorkflowTools(ctx, ownerID)...)
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
	// projectID is the session's bound project, applied to every
	// sandbox toolset built for this run — handoff-target agents included, so
	// one run sees one file system context throughout.
	projectID string
	// ownerID is the session owner every built config must be visible to
	// (decisions §5.29); empty skips the check (internal callers with no user).
	ownerID string
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

func buildAgentFromConfig(ctx context.Context, deps *AgentDeps, configID string, bc *agentBuildCtx) (*BuildResult, error) {
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
	// A foreign private config reads as absent (decisions §5.29): a run must never
	// execute — and spend the credentials of — another user's agent. Handoff
	// targets pass through here too, so the whole registry is covered.
	if bc.ownerID != "" && !store.Visible(ac.Scope, ac.OwnerID, bc.ownerID, false) {
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
		gs, gerr := deps.Guardrails.Build(ctx, ac.Guardrails.Guardrails)
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

	// Provider + retry/fallback decorators.
	proxyClient := deps.Settings.ProxyClient(ctx)
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
	result.Profile.MCPServerIDs = attachMCPServers(ctx, deps, agent, spec, bc.ownerID)

	mark := len(agent.Tools)

	// Sandbox tools — "" means none; a build failure fails the run. Also
	// appends the sandbox's own prompt to the instructions (measured into the
	// profile), so it lands after memories and before the skills index below.
	if err := attachSandboxTools(ctx, deps, bc, agent, approveCommands, &result.Profile); err != nil {
		return nil, err
	}
	mark = bucketToolsSince(agent, mark, store.ToolSourceSandbox, &result.Profile)

	// Skills — loaded from the store; spec may restrict the selection.
	result.Profile.SkillsIndexChars = attachSkills(ctx, deps, agent, spec, bc.ownerID)
	bucketToolsSince(agent, mark, store.ToolSourceSkills, &result.Profile)

	// Handoffs — recursively built; a target with its own provider gets its model
	// pre-resolved so the run uses the target's backend, not the main agent's.
	if err := buildHandoffs(ctx, deps, bc, agent, result, ac, spec); err != nil {
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
func buildHandoffs(ctx context.Context, deps *AgentDeps, bc *agentBuildCtx, agent *agents.Agent, result *BuildResult, ac *store.AgentConfig, spec *AgentSpec) error {
	for _, hID := range spec.Handoffs {
		hResult, err := buildAgentFromConfig(ctx, deps, hID, bc)
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
			return fmt.Errorf(
				"agent %q handoff %q: target is on the %q backend but reaches no API key, so it would inherit this agent's %q provider — point the target at a provider with a key",
				ac.Name, hID, hResult.ProviderType, result.ProviderType)
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
// skipping any whose server is not currently connected — or no longer
// visible to the session owner (decisions §5.29: a demoted server drops out like
// a deleted one, never serving another user's credentialed connection). It
// returns the ids it attached — the profile records the decision actually
// made, not a re-derivation that could race a reconnect.
func attachMCPServers(ctx context.Context, deps *AgentDeps, agent *agents.Agent, spec *AgentSpec, ownerID string) []string {
	var attached []string
	for _, id := range spec.Tools {
		if ownerID != "" && deps.McpServers != nil {
			cfg, err := deps.McpServers.Get(ctx, id)
			if err != nil || !store.Visible(cfg.Scope, cfg.OwnerID, ownerID, false) {
				logging.Ctx(ctx).Debug("MCP server not visible to this session, skipping", "mcp_id", id)
				continue
			}
		}
		if srv := deps.McpManager.Get(id); srv != nil {
			agent.MCPServers = append(agent.MCPServers, srv)
			attached = append(attached, id)
		} else {
			logging.Ctx(ctx).Debug("MCP server not connected, skipping", "mcp_id", id)
		}
	}
	return attached
}

// attachSandboxTools builds and attaches the bound project's sandbox tools
// when one is selected. NO PROJECT MEANS NO SANDBOX TOOLS: an agent without a
// working tree is a chat, not a workbench (decisions §5.33). A build failure
// fails the run rather than silently downgrading — a bound session must not
// run coding prompts with no file system. The instance reference is recorded
// on bc for the build's Release.
func attachSandboxTools(ctx context.Context, deps *AgentDeps, bc *agentBuildCtx, agent *agents.Agent, approveCommands bool, prof *store.PromptProfile) error {
	if bc.projectID == "" {
		return nil
	}
	if deps.Projects == nil {
		return fmt.Errorf("project %s: no project store is wired", bc.projectID)
	}
	proj, err := deps.Projects.Get(ctx, bc.projectID)
	if err != nil {
		return fmt.Errorf("project %s: %w", bc.projectID, err)
	}
	spec, err := ProjectSpec(ctx, deps, proj)
	if err != nil {
		return err
	}
	tools, release, err := deps.SandboxManager.SandboxTools(spec, approveCommands)
	if err != nil {
		return fmt.Errorf("project %q: building tools: %w", proj.Name, err)
	}
	bc.releases = append(bc.releases, release)
	agent.Tools = append(agent.Tools, tools...)
	// The sandbox's own prompt — what this machine is and how to use it — as a
	// SUFFIX, so it lands after the agent's instructions and memories and
	// before the skills index the caller adds next.
	if p := spec.Sandbox.Prompt; p != "" {
		agent.Instructions = agents.WrapInstructions(agent.Instructions, "", p)
		prof.SandboxPromptChars = len(p)
	}
	return nil
}

// attachSkills loads the stored skills and, when spec restricts the selection
// (by skill id), filters to the advertised ones; the rendered index pairs
// with a read_skill tool. read_skill gates on the advertised NAMES but
// resolves them own-over-global (decisions §5.29), so a same-named row of the
// owner's outside the selection can serve the content. Best-effort: a load
// error is skipped, not fatal. Returns the size of the index it added.
func attachSkills(ctx context.Context, deps *AgentDeps, agent *agents.Agent, spec *AgentSpec, ownerID string) int {
	// No owner, no view (mirror of attachMCPServers) — and an empty owner in
	// the scoped WHERE is a type error on PostgreSQL's uuid column.
	if deps.Skills == nil || ownerID == "" {
		return 0
	}
	// The visible set is the session owner's view: global skills plus their
	// own (decisions §5.29) — a selection id pointing outside it simply drops out,
	// the same as a deleted skill.
	stored, err := deps.Skills.ListMeta(ctx, ownerID, false)
	if err != nil {
		return 0
	}
	if spec.SkillsSet {
		allowed := make(map[string]bool, len(spec.Skills))
		for _, id := range spec.Skills {
			allowed[id] = true
		}
		filtered := stored[:0]
		for _, sk := range stored {
			if allowed[sk.ID] {
				filtered = append(filtered, sk)
			}
		}
		stored = filtered
	}
	if len(stored) == 0 {
		return 0
	}
	// The index advertises MODEL-FACING names: "owner/repo:name" for imported
	// skills, the bare name for workbench-authored ones. Two visible skills can
	// still share one (the caller's private import shadowing the same repo's
	// global group); the owner's wins: read_skill resolves own-over-global, so
	// the index entry's description must be the owned row's too, whichever
	// order ListMeta returned them in.
	index := make([]skills.Skill, 0, len(stored))
	pos := make(map[string]int, len(stored))
	for _, sk := range stored {
		name := sk.QualifiedName()
		if at, seen := pos[name]; seen {
			if store.Shadows(sk.Scope, sk.OwnerID, ownerID) {
				index[at].Description = sk.Description
			}
			continue
		}
		pos[name] = len(index)
		index = append(index, skills.Skill{Name: name, Description: sk.Description})
	}
	advertised := make(map[string]bool, len(pos))
	for name := range pos {
		advertised[name] = true
	}
	rendered := skills.RenderIndex(index)
	agent.Instructions = agents.WrapInstructions(agent.Instructions, "", rendered)
	agent.Tools = append(agent.Tools, readSkillTool(deps.Skills, advertised, ownerID))
	return len(rendered)
}

type readSkillArgs struct {
	Name string `json:"name" jsonschema:"the skill's name exactly as the skills index lists it, e.g. pdf-processing or anthropics/skills:pdf-processing"`
}

// readSkillTool serves a skill's full SKILL.md from the store by name —
// content is fetched at call time, never captured at build. Only skills whose
// index entry this agent carries are readable: the advertised set is the
// agent's skill selection, not the whole table.
func readSkillTool(store *store.SkillStore, advertised map[string]bool, ownerID string) *agents.Tool {
	t := agents.NewTool("read_skill",
		"Read a skill's full SKILL.md instructions by name.",
		func(ctx context.Context, _ *agents.ToolContext, args readSkillArgs) (string, error) {
			if !advertised[args.Name] {
				return "", fmt.Errorf("no skill named %q", args.Name)
			}
			sk, err := store.GetByNameFor(ctx, args.Name, ownerID)
			if err != nil {
				return "", fmt.Errorf("no skill named %q", args.Name)
			}
			return sk.Content, nil
		})
	t.ReadOnly = true // consulting a skill must survive plan mode
	return t
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

// ValidateAgentToolNames simulates the statically knowable part of an agent's
// final tool list and reports name collisions that the SDK would otherwise
// reject only at run time (duplicate tool names are a UserError). Every MCP
// server's tools get the "<server name>__" prefix, so selecting the same
// server twice — or two servers sharing a name, legal since names are unique
// per SCOPE — is a guaranteed collision. The servers' actual tool lists are
// only known once connected and cannot be validated here.
func ValidateAgentToolNames(ctx context.Context, mcpServers *store.McpServerStore, toolsJSON string) error {
	if toolsJSON == "" || mcpServers == nil {
		return nil
	}
	// A malformed tools list is rejected here (and at build time) rather than
	// silently dropping every MCP tool the agent was meant to have.
	var ids []string
	if err := json.Unmarshal([]byte(toolsJSON), &ids); err != nil {
		return fmt.Errorf("tools selection is invalid: %w", err)
	}
	seenID := map[string]bool{}
	prefixOwner := map[string]string{}
	for _, id := range ids {
		cfg, err := mcpServers.Get(ctx, id)
		if err != nil {
			continue // unknown/removed servers are skipped when the agent is built
		}
		if seenID[id] {
			return fmt.Errorf("MCP server %q is selected twice; each of its tools would appear under the same name twice", cfg.Name)
		}
		seenID[id] = true
		if prev, ok := prefixOwner[cfg.Name]; ok && prev != id {
			return fmt.Errorf("two selected MCP servers are named %q; their tools would collide under one prefix", cfg.Name)
		}
		prefixOwner[cfg.Name] = id
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

// ProjectSpec loads the sandbox a project names — everything the sandbox
// manager needs to build or acquire its instance.
func ProjectSpec(ctx context.Context, deps *AgentDeps, proj *store.Project) (sandboxes.Spec, error) {
	sb, err := deps.Sandboxes.Get(ctx, proj.SandboxID)
	if err != nil {
		return sandboxes.Spec{}, fmt.Errorf("project %q: sandbox %s: %w", proj.Name, proj.SandboxID, err)
	}
	return sandboxes.Spec{Sandbox: sb, Project: proj}, nil
}
