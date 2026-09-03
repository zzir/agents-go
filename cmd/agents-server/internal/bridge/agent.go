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
	// Attachments resolves image-attachment sentinels at the model boundary
	// (see hydratingProvider) and validates a run's attachment_ids.
	Attachments *store.AttachmentStore
	// Audit records the acts a run performs on shared configuration (a
	// save_workflow), attributed to the session's owner; nil records nothing.
	Audit protocol.AuditFunc
	// Users answers the run owner's role; nil withholds the tools only an
	// admin gets.
	Users *store.UserStore
	// TaskManager is set by NewRunner; when non-nil, chat agents get the task
	// tools. A BACKGROUND run never gets them — invariant 34.
	TaskManager *tasks.Manager
	// SpawnTool is set by NewRunner and builds the run's spawn_task per run (the
	// workflows on offer change without a restart); never on a background run.
	SpawnTool func(ctx context.Context, ownerID string) *agents.Tool
	// WorkflowTools is set by NewRunner and builds get_workflow / save_workflow
	// per run, when the config opts in (behavior.workflow_authoring) — invariant 39.
	WorkflowTools func(ctx context.Context, ownerID string) []*agents.Tool
}

// BuildResult contains the built agent and its resolved model provider.
type BuildResult struct {
	Agent    *agents.Agent
	Provider agents.ModelProvider

	// AgentIDs maps each built agent's name to its config id, carried on the
	// events that announce an agent by name. Set only on the entry build.
	AgentIDs map[string]string

	// Behavior, Compaction and Session are the config's groups AS STORED; a knob
	// the build converted has a "Derived:" field below (StopAtTools and
	// ReasoningItemIDPolicy keep their name here and change type there).
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
	// response content (trace_include_sensitive_data); off, Replay has no seed.
	TraceIncludeSensitive bool

	// LogSensitive gates conversation content in the SDK's own log records
	// (log_sensitive_data) — a separate switch from the trace one.
	LogSensitive bool

	// PlanPhase is set when built in plan mode: the run starts read-only and the
	// approved submit_plan unlocks it (spec §2.12).
	PlanPhase *middleware.PlanPhase

	// ReasoningItemIDPolicy controls whether reasoning-item ids survive across
	// turns (default preserve). Derived: Behavior stores it as a string.
	ReasoningItemIDPolicy agents.ReasoningItemIDPolicy

	// StopAtTools ends the run after a turn that called any of these tools.
	// Empty means the run continues until the model stops on its own.
	// Derived: Behavior stores it as one comma-separated string.
	StopAtTools []string

	// RunGuardrails are the entry agent's guardrails lifted to the RUN level (so
	// the final output is covered after a handoff); cleared off the root agent.
	RunGuardrails []agents.Guardrail

	// Profile is what this build put in front of the model (instruction layers
	// and tool surface, in characters), for the Context panel; ENTRY agent only.
	Profile store.PromptProfile

	// releaseSandbox drops every sandbox-instance reference this build acquired
	// (entry and handoff targets, folded into one); nil without a sandbox.
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

// BuildFullAgent constructs an *agents.Agent from a config id with everything
// it names: provider, MCP tools, the project's sandbox tools, memories, skills
// and the global system prompt. forUserID is who the build is for, as a run's
// owner would be: it decides which tools they get.
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

// buildFullAgent is BuildFullAgent with the run-scoped extras: the bound
// project, BACKGROUND (invariant 34), and the owner whose role gates saves.
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
	if err == nil && !background && deps.TaskManager != nil && result.Behavior.SubagentsOn() {
		// The model's background surface, never for a background run (invariant
		// 34); an agent may opt out (behavior.subagents=false).
		mark := len(result.Agent.Tools)
		if deps.SpawnTool != nil {
			result.Agent.Tools = append(result.Agent.Tools, deps.SpawnTool(ctx, ownerID))
		} else {
			result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.SpawnTool(nil))
		}
		result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.TaskTools(nil)...)
		bucketToolsSince(result.Agent, mark, store.ToolSourceTasks, &result.Profile)
	}
	// Workflow authoring is opt-in per agent and chat-only — invariant 39.
	if err == nil && !background && result.Behavior.WorkflowAuthoring && deps.WorkflowTools != nil {
		mark := len(result.Agent.Tools)
		// Every owner may save; only a global workflow's edit stays the admin's
		// (saveWorkflow decides per call, mirroring the REST gate).
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
	// Told, not merely arranged for — invariant 34. A SUFFIX, so it lands after
	// the agent's own instructions, which may well say to ask.
	if background && result.Agent != nil {
		result.Agent.Instructions = agents.WrapInstructions(result.Agent.Instructions, "", BackgroundInstructions)
	}
	// The entry agent's guardrails move to the run level (cleared off the root
	// so they run once); handoff targets keep their own.
	if result.Agent != nil {
		result.RunGuardrails = result.Agent.Guardrails
		result.Agent.Guardrails = nil
	}
	// Plan/todo rewrite the ENTRY agent at BUILD time (spec §2.12), last so the
	// gates cover the task tools; unconditional (invariant 33); never background.
	if !background && result.Agent != nil {
		mark := len(result.Agent.Tools)
		result.Agent = middleware.Todo{}.Apply(result.Agent)
		mark = bucketToolsSince(result.Agent, mark, store.ToolSourceTodo, &result.Profile)
		result.Agent, result.PlanPhase = middleware.Plan{}.Apply(result.Agent)
		bucketToolsSince(result.Agent, mark, store.ToolSourcePlan, &result.Profile)
	}
	return result, nil
}

// agentBuildCtx threads a recursive handoff build: stack is the recursion PATH
// (cycle detection — a diamond's shared node is no back-edge), cache reuses builds.
type agentBuildCtx struct {
	stack map[string]bool
	cache map[string]*BuildResult
	// projectID is the session's bound project, applied to every sandbox
	// toolset of this run — handoff targets included.
	projectID string
	// ownerID is the session owner every built config must be visible to
	// (decisions §5.29); empty skips the check (internal callers with no user).
	ownerID string
	// releases collects every sandbox reference the recursion acquired, on the
	// CONTEXT: only the top-level BuildResult reaches the caller.
	releases []func()
}

// errHandoffCycle marks a back-edge in the handoff graph — recoverable: the
// edge is dropped and the rest builds.
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
	// A foreign private config reads as absent (decisions §5.29); handoff
	// targets pass through here too.
	if bc.ownerID != "" && !store.Visible(ac.Scope, ac.OwnerID, bc.ownerID, false) {
		return nil, fmt.Errorf("agent config %q not found — create one in Settings > Agents", configID)
	}

	agent := &agents.Agent{
		Name:                   ac.Name,
		Model:                  ac.Model,
		HandoffDescription:     ac.Behavior.HandoffDescription,
		DisableToolChoiceReset: !ac.Behavior.ToolChoiceResetOn(),
	}
	result.Behavior = ac.Behavior
	result.Compaction = ac.Compaction
	result.Session = ac.Session
	if ac.Behavior.ReasoningItemIDPolicy == "omit" {
		result.ReasoningItemIDPolicy = agents.ReasoningItemIDOmit
	}

	// Every JSON field is decoded once, up front; DecodeAgentSpec also backs
	// save-time validation.
	spec, err := DecodeAgentSpec(ac)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", ac.Name, err)
	}

	if ac.Instructions != "" {
		agent.Instructions = agents.StaticInstructions(ac.Instructions)
	}
	agent.ModelSettings = spec.ModelSettings
	result.ErrorHandlers = spec.ErrorHandlers.BuildErrorHandlers()

	result.StopAtTools = settings.SplitList(ac.Behavior.StopAtTools)

	// A guardrail that can't be resolved fails the build rather than running
	// unprotected — invariant 13.
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

	// Every layer below is also MEASURED into result.Profile as it is added
	// (see store.ContextProfile).
	layerInstructions(ctx, deps, agent, ac, &result.Profile)

	// MCP servers — a server not connected is skipped. Their tools are not
	// measured here: asking is a network call (see the Context handler).
	result.Profile.MCPServerIDs = attachMCPServers(ctx, deps, agent, spec, bc.ownerID)

	mark := len(agent.Tools)

	// Sandbox tools — "" means none; a build failure fails the run. Also appends
	// the sandbox's own prompt (after memories, before the skills index).
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

// splitApproveTools reads the approve list: "exec_command" opts into the
// per-command session gate and is kept OUT of the SDK's ApproveTools.
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

// buildHandoffs builds each handoff target recursively (a cycle drops the edge;
// other failures propagate); a keyless target on another backend is refused.
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

// attachMCPServers wires the selected MCP servers, skipping any not connected
// or not visible to the owner (decisions §5.29); returns the ids attached.
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

// attachSandboxTools attaches the bound project's sandbox tools. NO PROJECT,
// NO SANDBOX TOOLS (decisions §5.33); a build failure fails the run.
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
	spec, err := projectSpec(ctx, deps, proj)
	if err != nil {
		return err
	}
	tools, release, err := deps.SandboxManager.SandboxTools(spec, approveCommands)
	if err != nil {
		return fmt.Errorf("project %q: building tools: %w", proj.Name, err)
	}
	bc.releases = append(bc.releases, release)
	agent.Tools = append(agent.Tools, tools...)
	// The sandbox's own prompt as a SUFFIX: after the agent's instructions and
	// memories, before the skills index the caller adds next.
	if p := spec.Sandbox.Prompt; p != "" {
		agent.Instructions = agents.WrapInstructions(agent.Instructions, "", p)
		prof.SandboxPromptChars = len(p)
	}
	return nil
}

// attachSkills renders the visible skills' index (filtered by spec's selection)
// with a read_skill tool that resolves own-over-global (decisions §5.29).
func attachSkills(ctx context.Context, deps *AgentDeps, agent *agents.Agent, spec *AgentSpec, ownerID string) int {
	// No owner, no view (mirror of attachMCPServers) — and an empty owner in
	// the scoped WHERE is a type error on PostgreSQL's uuid column.
	if deps.Skills == nil || ownerID == "" {
		return 0
	}
	// The owner's view: global plus their own (decisions §5.29); a selection id
	// outside it drops out like a deleted skill.
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
	// The index advertises MODEL-FACING names (QualifiedName); when two visible
	// skills share one, the owner's description wins, as read_skill resolves.
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

// readSkillTool serves a skill's SKILL.md from the store at call time; only
// skills whose index entry this agent carries are readable.
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

// bucketToolsSince records the tools appended since mark under source and
// returns the new mark. Positional: the build knows which step added what.
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

// Provider selection — validation, construction, auth modes — lives in
// internal/providers/registry.go; nothing here switches on a provider type.

// ownerIsAdmin reports whether the run's owner may write shared configuration;
// anything that cannot say yes says no.
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

// projectSpec loads the sandbox a project names — everything the sandbox
// manager needs to build or acquire its instance.
func projectSpec(ctx context.Context, deps *AgentDeps, proj *store.Project) (sandboxes.Spec, error) {
	sb, err := deps.Sandboxes.Get(ctx, proj.SandboxID)
	if err != nil {
		return sandboxes.Spec{}, fmt.Errorf("project %q: sandbox %s: %w", proj.Name, proj.SandboxID, err)
	}
	return sandboxes.Spec{Sandbox: sb, Project: proj}, nil
}
