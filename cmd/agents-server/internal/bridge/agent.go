// Package bridge adapts stored configuration into live agents-SDK constructs (agents, model providers, MCP servers, sandboxes, guardrails) and drives streamed runs over the WebSocket.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3/option"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
	"github.com/zzir/agents-go/tools/bravesearch"
)

// AgentDeps holds all the dependencies needed to build a fully configured agent.
type AgentDeps struct {
	AgentConfigs     *store.AgentConfigStore
	McpServers       *store.McpServerStore
	SandboxConfigs   *store.SandboxStore
	Memories         *store.MemoryStore
	Settings         *store.SettingStore
	ProviderRoutes   *store.ProviderRouteStore
	Sessions         *store.SessionStore
	Traces           *store.TraceStore
	Guardrails       *GuardrailResolver
	McpManager       *McpManager
	SandboxManager   *SandboxManager
	ChatGPTOAuth     *ChatGPTOAuth
	PendingApprovals *store.PendingApprovalStore
	Tasks            *store.TaskStore
	Workspace        string
	// MaxTasks overrides the per-session live background-task cap when > 0
	// (--max-tasks; 0 keeps the built-in default).
	MaxTasks int
	// TaskManager is set by NewRunner; when non-nil, chat agents get the
	// spawn_task / task_status / task_stop tools. A TASK's own run never gets
	// them — that is what bounds recursion — so the caller passes taskRun.
	TaskManager *tasks.Manager
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
}

// BuildFullAgent constructs an *agents.Agent from a config ID, loading all
// associated resources: provider, MCP tools, sandbox CodeTool, memory, and
// global settings (system_prompt). agentConfigID is required. sandboxID is
// optional — when set, only that sandbox is attached; when empty, all are.
func BuildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, sandboxID string) (*BuildResult, error) {
	return buildFullAgent(ctx, deps, agentConfigID, sandboxID, false)
}

// buildFullAgent is BuildFullAgent with task-run awareness: a background task
// run's agent is built WITHOUT the task tools, capping spawn depth at one.
func buildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, sandboxID string, taskRun bool) (*BuildResult, error) {
	if agentConfigID == "" {
		return nil, fmt.Errorf("agent_config_id is required")
	}
	bc := &agentBuildCtx{
		stack: make(map[string]bool),
		cache: make(map[string]*BuildResult),
	}
	result, err := buildAgentFromConfig(ctx, deps, agentConfigID, sandboxID, bc)
	if err == nil {
		result.TraceIncludeSensitive = sensitiveTraceSetting(ctx, deps.Settings)
	}
	if err == nil && !taskRun && deps.TaskManager != nil {
		// The session id reaches the tools through the run context, not the
		// model: otherwise one conversation could spawn tasks onto another.
		result.Agent.Tools = append(result.Agent.Tools, deps.TaskManager.Tools(nil)...)
	}
	if err != nil {
		return nil, err
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
	// above. Task runs are excluded: nobody sits on the other side of a
	// background task's plan review.
	if !taskRun && result.Agent != nil {
		if result.Behavior.TodoList {
			result.Agent = middleware.Todo{}.Apply(result.Agent)
		}
		if result.Behavior.PlanMode {
			result.Agent, result.PlanPhase = middleware.Plan{}.Apply(result.Agent)
		}
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

	log := zerolog.Ctx(ctx)
	result := &BuildResult{}

	globalSystemPrompt := settingValue(ctx, deps.Settings, "system_prompt")

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

	// Decode every JSON-encoded config field once, up front. A structural error
	// fails the build loudly (the operator would otherwise think a malformed
	// guardrail / schema / settings block took effect); the same decode backs
	// save-time validation, so the contract lives in one place (DecodeAgentSpec).
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

	// HITL tool approval and structured-output schema — decoded in spec.
	// "exec_command" in the approve list opts INTO per-command session approval,
	// but must not ride the SDK's ApproveTools OR — that forces approval on every
	// call and defeats session trust. Route it to the sandbox command gate
	// instead and strip it from the SDK list.
	approveCommands := false
	if len(spec.ApproveTools) > 0 {
		filtered := make([]string, 0, len(spec.ApproveTools))
		for _, name := range spec.ApproveTools {
			if name == "exec_command" {
				approveCommands = true
				continue
			}
			filtered = append(filtered, name)
		}
		if len(filtered) > 0 {
			agent.ApproveTools = filtered
		}
	}
	agent.OutputType = spec.OutputType

	// Stored prompt
	if ac.Session.PromptID != "" {
		agent.Prompt = agents.StaticPrompt(agents.Prompt{
			ID:      ac.Session.PromptID,
			Version: ac.Session.PromptVersion,
		})
	}

	// Provider + retry/fallback decorators
	if err := ValidateProviderSelection(ac); err != nil {
		return nil, fmt.Errorf("agent %q: %w", ac.Name, err)
	}
	def, err := providerDefFor(ac.Provider.ProviderType)
	if err != nil {
		return nil, err // unreachable after validation; fail loud, never default
	}
	proxyClient := ProxyHTTPClient(ctx, deps.Settings)
	result.ProviderType = def.Type
	apiKey := ac.Provider.APIKey
	var chatgptCreds *ChatGPTCredentials
	// Validation limits chatgpt_login to backends that list it, so no
	// provider check is needed here.
	if ac.Provider.AuthMode == authModeChatGPTLogin && deps.ChatGPTOAuth != nil {
		if creds, err := deps.ChatGPTOAuth.GetCredentials(ctx, configID); err == nil {
			apiKey = creds.AccessToken
			chatgptCreds = creds
		} else {
			log := zerolog.Ctx(ctx)
			log.Warn().Err(err).Msg("ChatGPT OAuth token unavailable, falling back to api_key")
		}
	}
	if apiKey == "" {
		// The global per-provider fallback key (derived into
		// handler.secretSettingKeys from the same registry).
		apiKey = settingValue(ctx, deps.Settings, def.SettingKey)
	}
	if apiKey != "" {
		ac.Provider.APIKey = apiKey
		if chatgptCreds != nil && ac.Provider.BaseURL == "" {
			ac.Provider.BaseURL = ChatGPTBaseURL
		}
		provider := def.Build(ac.Provider.APIKey, ac.Provider.BaseURL, chatgptCreds, proxyClient)
		if ac.Resilience.RetryEnabled {
			provider = agents.NewRetryProvider(provider, spec.RetryPolicy)
		}
		if len(spec.FallbackModels) > 0 {
			provider = wrapFallbackProvider(provider, spec.FallbackModels, proxyClient, func(providerType string) string {
				fdef, ferr := providerDefFor(providerType)
				if ferr != nil {
					return ""
				}
				return settingValue(ctx, deps.Settings, fdef.SettingKey)
			})
		}
		result.Provider = provider
	}

	// Global system prompt
	if globalSystemPrompt != "" {
		agent.Instructions = agents.WrapInstructions(agent.Instructions, globalSystemPrompt, "")
	}

	// Memory
	memories, err := deps.Memories.ListForAgent(ctx, configID)
	if err == nil && len(memories) > 0 {
		agent.Instructions = agents.WrapInstructions(agent.Instructions, "", buildMemoryBlock(memories))
	}

	// MCP servers — the selected ids are decoded in spec; an id whose server
	// isn't currently connected is skipped (the tool set reflects live state).
	for _, id := range spec.Tools {
		srv := deps.McpManager.Get(id)
		if srv != nil {
			agent.MCPServers = append(agent.MCPServers, srv)
		} else {
			log.Debug().Str("mcp_id", id).Msg("MCP server not connected, skipping")
		}
	}

	// Sandbox tools: "" = none, else = specific ID
	if sandboxID != "" {
		sbCfg, err := deps.SandboxConfigs.Get(ctx, sandboxID)
		if err == nil {
			tools, err := deps.SandboxManager.SandboxTools(sbCfg, approveCommands)
			if err != nil {
				log.Warn().Err(err).Str("sandbox", sbCfg.Name).Msg("failed to create sandbox tools")
			} else {
				agent.Tools = append(agent.Tools, tools...)
			}
		}
	}

	// Brave Search
	if apiKey := settingValue(ctx, deps.Settings, "brave_api_key"); apiKey != "" {
		bsOpts := bravesearch.Options{APIKey: apiKey}
		if proxyClient != nil {
			bsOpts.HTTPClient = proxyClient
		}
		bsTool, err := bravesearch.New(bsOpts)
		if err == nil {
			agent.Tools = append(agent.Tools, bsTool)
		} else {
			log.Warn().Err(err).Msg("failed to create brave_search tool")
		}
	}

	// Skills — loaded from <root>/skills, matching where SkillHandler manages
	// them. Load and ReadFileTool must share this root so the relative paths in
	// the rendered index resolve correctly. ac.SkillsJSON, when set, restricts
	// which loaded skills are advertised to this agent (matched by Dir, e.g.
	// "docx" or "some-repo/docx" — the same directory-relative id the Skills
	// API and the Agent form's checkboxes use); an unset SkillsJSON (agents
	// that pre-date per-agent scoping) still gets every installed skill.
	if deps.Workspace != "" {
		skillsDir := filepath.Join(deps.Workspace, "skills")
		loadedSkills, err := skills.LoadRecursive(skillsDir)
		if err == nil && spec.SkillsSet {
			// spec.Skills restricts which loaded skills this agent advertises;
			// an unset selection (SkillsSet == false) keeps every installed skill.
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
		if err == nil && len(loadedSkills) > 0 {
			agent.Instructions = agents.WrapInstructions(agent.Instructions, "", skills.RenderIndex(loadedSkills))
			agent.Tools = append(agent.Tools, skills.ReadFileTool(skillsDir))
		}
	}

	// Handoffs — recursive build over the decoded target ids. Each target that
	// carries its own provider gets its model pre-resolved into ModelImpl so the
	// run loop uses the target's backend, not the run-level (main agent's) one.
	if len(spec.Handoffs) > 0 {
		for _, hID := range spec.Handoffs {
			hResult, err := buildAgentFromConfig(ctx, deps, hID, sandboxID, bc)
			if err != nil {
				// A cycle is recoverable: drop the back-edge and keep going.
				// Any other failure means the target's config is broken, and
				// silently dropping the handoff would hide it — propagate.
				if errors.Is(err, errHandoffCycle) {
					log.Warn().Err(err).Str("handoff_id", hID).Msg("handoff cycle, skipping edge")
					continue
				}
				return nil, fmt.Errorf("agent %q handoff %q: %w", ac.Name, hID, err)
			}
			// A keyless target has no provider of its own and would resolve
			// through the RUN's provider at handoff time. Same backend, that
			// is credential sharing; a different backend would silently send
			// the target's model name to the wrong API — refuse it instead.
			if hResult.Provider == nil && hResult.ProviderType != result.ProviderType {
				targetKey := hResult.ProviderType + "_api_key"
				if tdef, terr := providerDefFor(hResult.ProviderType); terr == nil {
					targetKey = tdef.SettingKey
				}
				return nil, fmt.Errorf(
					"agent %q handoff %q: target uses provider_type %q but has no API key, so it would inherit this agent's %q provider — give the target its own api_key or set the global %s",
					ac.Name, hID, hResult.ProviderType, result.ProviderType, targetKey)
			}
			if hResult.Provider != nil && hResult.Agent.Model != "" && hResult.Agent.ModelImpl == nil {
				m, merr := hResult.Provider.Model(hResult.Agent.Model)
				if merr != nil {
					return nil, fmt.Errorf("agent %q handoff %q: resolve model: %w", ac.Name, hID, merr)
				}
				hResult.Agent.ModelImpl = m
			}
			agent.Handoffs = append(agent.Handoffs, agents.HandoffTo(hResult.Agent))
		}
	}

	result.Agent = agent
	bc.cache[configID] = result
	return result, nil
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

func settingValue(ctx context.Context, settings *store.SettingStore, key string) string {
	if settings == nil {
		return ""
	}
	s, err := settings.Get(ctx, key)
	if err != nil || s.Value == "" {
		return ""
	}
	return s.Value
}

// sensitiveTraceSetting reads the global trace_include_sensitive_data setting
// as the tri-state the SDK expects: nil (unset / unparsable) defers to the SDK
// default of including everything; an explicit false keeps prompts, outputs
// and tool arguments out of stored traces.
func sensitiveTraceSetting(ctx context.Context, settings *store.SettingStore) *bool {
	raw := strings.TrimSpace(settingValue(ctx, settings, "trace_include_sensitive_data"))
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &v
}

// Provider selection — validation, construction, auth modes, setting keys —
// lives in the registry (provider_registry.go); nothing here should switch on
// a provider type.

func newChatGPTMiddleware(accountID string) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if accountID != "" {
			req.Header.Set("ChatGPT-Account-ID", accountID)
		}
		req.Header.Set("originator", "codex_cli_rs")

		if req.Body != nil && req.Method == http.MethodPost {
			raw, err := io.ReadAll(req.Body)
			req.Body.Close()
			if err == nil {
				var body map[string]any
				if json.Unmarshal(raw, &body) == nil {
					body["store"] = false
					delete(body, "previous_response_id")
					if input, ok := body["input"].([]any); ok {
						body["input"] = sanitizeChatGPTInput(input)
					}
					patched, _ := json.Marshal(body)
					raw = patched
				}
				req.Body = io.NopCloser(strings.NewReader(string(raw)))
				req.ContentLength = int64(len(raw))
			}
		}

		return next(req)
	}
}

type fallbackEntry struct {
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	// Provider selects the backend ("openai" / "anthropic"); empty is openai.
	// The JSON key is provider_type, matching the agent config group and
	// provider routes — one spelling across all three selector surfaces.
	Provider string `json:"provider_type"`
}

// fixedModelProvider pins a provider to one model name, ignoring the name the
// run requests. The SDK's FallbackProvider asks every fallback for the SAME
// (primary) model name, so without this a configured fallback model would never
// be used — the fallback would just retry the primary's model name elsewhere.
type fixedModelProvider struct {
	inner agents.ModelProvider
	model string
}

func (f fixedModelProvider) Model(string) (agents.Model, error) {
	return f.inner.Model(f.model)
}

// wrapFallbackProvider chains one fixed-model provider per decoded fallback
// entry behind primary. The entries are decoded up front (DecodeAgentSpec), so
// this is pure construction — callers gate it on len(entries) > 0. keyFor
// resolves the global per-provider fallback key ("openai_api_key" /
// "anthropic_api_key") for entries that carry none of their own, the same
// courtesy the main agent gets.
func wrapFallbackProvider(primary agents.ModelProvider, entries []fallbackEntry, proxyClient *http.Client, keyFor func(providerType string) string) agents.ModelProvider {
	var fallbacks []agents.ModelProvider
	for _, e := range entries {
		apiKey := e.APIKey
		if apiKey == "" && keyFor != nil {
			apiKey = keyFor(e.Provider)
		}
		fp, err := buildPlainProvider(e.Provider, apiKey, e.BaseURL, proxyClient)
		if err != nil {
			// Unreachable through normal flow — DecodeAgentSpec validates every
			// entry's provider — and an unbuildable entry must not become an
			// OpenAI default, so it is left out of the chain.
			continue
		}
		if e.Model != "" {
			fp = fixedModelProvider{inner: fp, model: e.Model}
		}
		fallbacks = append(fallbacks, fp)
	}
	return agents.NewFallbackProvider(primary, fallbacks...)
}

// BuildRouterProvider builds a RouterProvider from all stored provider routes.
func BuildRouterProvider(ctx context.Context, deps *AgentDeps, fallback agents.ModelProvider) agents.ModelProvider {
	if deps.ProviderRoutes == nil {
		return fallback
	}
	routes, err := deps.ProviderRoutes.List(ctx)
	if err != nil {
		// Loud, because the silent version was observed: an unreadable table
		// (e.g. a pre-provider_type database) would otherwise disable ALL
		// routing with no signal, and every prefixed model name would fall to
		// the agent's own provider.
		zerolog.Ctx(ctx).Warn().Err(err).Msg("provider routes unavailable; prefix routing disabled for this run")
		return fallback
	}
	if len(routes) == 0 {
		return fallback
	}
	proxyClient := ProxyHTTPClient(ctx, deps.Settings)
	routeMap := make(map[string]agents.ModelProvider, len(routes))
	for _, r := range routes {
		// The save path validates provider_type, but a row can predate the
		// validation or bypass the API. An unregistered value must not default
		// to OpenAI — the silent wrong-backend case — so the route is skipped
		// loudly instead.
		fp, err := buildPlainProvider(r.ProviderType, r.APIKey, r.BaseURL, proxyClient)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("prefix", r.Prefix).Msg("provider route skipped: invalid provider_type")
			continue
		}
		routeMap[r.Prefix] = fp
	}
	if len(routeMap) == 0 {
		return fallback
	}
	router := agents.NewRouterProvider(routeMap)
	if fallback != nil {
		router.WithFallback(fallback)
	}
	return router
}

// splitList parses a comma-separated config value into trimmed, non-empty
// entries. Operators type these by hand, so stray spaces and trailing commas
// are normalized rather than turned into tool names that match nothing.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
