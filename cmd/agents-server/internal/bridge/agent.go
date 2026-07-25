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
	"strings"

	"github.com/openai/openai-go/v3/option"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	openaiProvider "github.com/zzir/agents-go/models/openai"
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
	// TaskSpawner is set by NewRunner; when non-nil, chat agents get the
	// spawn_task / task_status / task_stop tools.
	TaskSpawner TaskSpawner
}

// BuildResult contains the built agent and its resolved model provider.
type BuildResult struct {
	Agent                 *agents.Agent
	Provider              agents.ModelProvider
	MaxTurns              int
	UsePreviousResponseID bool
	HandoffInputFilter    string
	MaxToolConcurrency    int
	ToolNotFoundBehavior  string
	// ErrorHandlers are the run-level recovery handlers built from the
	// top-level config's error_handlers field (zero value when unconfigured).
	ErrorHandlers       agents.RunErrorHandlers
	CompactionEnabled   bool
	CompactionThreshold int
	CompactionWindow    int
	CompactionModel     string
	CompactionPrompt    string

	// HistoryLimit caps how many recent session items each turn loads (0 = all).
	HistoryLimit int

	// ReasoningItemIDPolicy controls whether reasoning-item ids survive across
	// turns (default preserve).
	ReasoningItemIDPolicy agents.ReasoningItemIDPolicy

	// HandoffToolNames is the set of every transfer_to_* tool name across the
	// whole built agent graph (root + all reachable handoff targets). The stream
	// bridge uses it to drop the tool_called event the SDK now emits for a
	// handoff, which would otherwise render as a tool card that never completes.
	HandoffToolNames map[string]bool

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
	if err == nil && !taskRun && deps.TaskSpawner != nil {
		result.Agent.Tools = append(result.Agent.Tools, TaskTools(deps.TaskSpawner)...)
	}
	if err != nil {
		return nil, err
	}
	// Collect every handoff transfer tool name across the whole graph — the cache
	// holds each built agent (root + shared/diamond descendants) exactly once —
	// so the stream bridge can suppress the SDK's handoff tool_called at any depth.
	handoffNames := map[string]bool{}
	for _, br := range bc.cache {
		if br.Agent == nil {
			continue
		}
		for _, h := range br.Agent.Handoffs {
			handoffNames[h.ToolName] = true
		}
	}
	result.HandoffToolNames = handoffNames
	// Lift the entry agent's guardrails to the run level so they protect the
	// whole conversation — the final output is checked even after a handoff to an
	// agent that carries no guardrails of its own. Cleared off the root agent so
	// they run once (the SDK merges run-level + producing-agent guardrails).
	// Handoff targets, built recursively, keep their own agent-level guardrails.
	if result.Agent != nil {
		result.RunGuardrails = result.Agent.Guardrails
		result.Agent.Guardrails = nil
	}
	// Safety net for configs saved before the API started rejecting the flag:
	// agents-server always runs with a persisted session, and the SDK refuses
	// Session + UsePreviousResponseID. Only the top-level config's flag is ever
	// forwarded to RunOptions, so handoff targets are not checked here.
	if result.UsePreviousResponseID {
		return nil, fmt.Errorf(
			"agent %q has use_previous_response_id enabled, which is incompatible with the server's session storage — edit the agent and disable use_previous_response_id",
			result.Agent.Name)
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
	result.MaxTurns = ac.Behavior.MaxTurns
	result.UsePreviousResponseID = ac.Session.UsePreviousResponseID
	result.HandoffInputFilter = ac.Behavior.HandoffInputFilter
	result.MaxToolConcurrency = ac.Behavior.MaxToolConcurrency
	result.ToolNotFoundBehavior = ac.Behavior.ToolNotFoundBehavior
	result.CompactionEnabled = ac.Compaction.Enabled
	result.CompactionThreshold = ac.Compaction.Threshold
	result.CompactionWindow = ac.Compaction.Window
	result.CompactionModel = ac.Compaction.Model
	result.CompactionPrompt = ac.Compaction.Prompt
	result.HistoryLimit = ac.Session.HistoryLimit
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

	// ToolUseBehavior
	agent.ToolUseBehavior = agents.ParseToolUseBehavior(ac.Behavior.ToolUseBehavior)

	// Guardrails — a configured guardrail that can't be resolved fails the
	// build rather than running unprotected (security config must not silently
	// no-op).
	if ac.Guardrails.InputGuardrails != "" && deps.Guardrails != nil {
		ig, gerr := deps.Guardrails.BuildInputGuardrails(ctx, ac.Guardrails.InputGuardrails)
		if gerr != nil {
			return nil, fmt.Errorf("agent %q: %w", ac.Name, gerr)
		}
		agent.Guardrails = append(agent.Guardrails, ig...)
	}
	if ac.Guardrails.OutputGuardrails != "" && deps.Guardrails != nil {
		og, gerr := deps.Guardrails.BuildOutputGuardrails(ctx, ac.Guardrails.OutputGuardrails)
		if gerr != nil {
			return nil, fmt.Errorf("agent %q: %w", ac.Name, gerr)
		}
		agent.Guardrails = append(agent.Guardrails, og...)
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
	proxyClient := ProxyHTTPClient(ctx, deps.Settings)
	apiKey := ac.Provider.APIKey
	var chatgptCreds *ChatGPTCredentials
	if ac.Provider.AuthMode == "chatgpt_login" && deps.ChatGPTOAuth != nil {
		if creds, err := deps.ChatGPTOAuth.GetCredentials(ctx, configID); err == nil {
			apiKey = creds.AccessToken
			chatgptCreds = creds
		} else {
			log := zerolog.Ctx(ctx)
			log.Warn().Err(err).Msg("ChatGPT OAuth token unavailable, falling back to api_key")
		}
	}
	if apiKey == "" {
		apiKey = settingValue(ctx, deps.Settings, "openai_api_key")
	}
	if apiKey != "" {
		ac.Provider.APIKey = apiKey
		if chatgptCreds != nil && ac.Provider.BaseURL == "" {
			ac.Provider.BaseURL = ChatGPTBaseURL
		}
		provider := buildProviderFromConfig(ac, chatgptCreds, proxyClient)
		if ac.Resilience.RetryEnabled {
			provider = agents.NewRetryProvider(provider, spec.RetryPolicy)
		}
		if len(spec.FallbackModels) > 0 {
			provider = wrapFallbackProvider(provider, spec.FallbackModels, proxyClient)
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
			if hResult.Provider != nil && hResult.Agent.Model != "" && hResult.Agent.ModelImpl == nil {
				m, merr := hResult.Provider.GetModel(hResult.Agent.Model)
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

func buildProviderFromConfig(ac *store.AgentConfig, chatgptCreds *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider {
	var opts []option.RequestOption
	if ac.Provider.APIKey != "" {
		opts = append(opts, option.WithAPIKey(ac.Provider.APIKey))
	}
	if ac.Provider.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(ac.Provider.BaseURL))
	}
	if chatgptCreds != nil {
		opts = append(opts, option.WithMiddleware(newChatGPTMiddleware(chatgptCreds.AccountID)))
	}
	if proxyClient != nil {
		opts = append(opts, option.WithHTTPClient(proxyClient))
	}
	return openaiProvider.NewProvider(opts...)
}

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
}

// fixedModelProvider pins a provider to one model name, ignoring the name the
// run requests. The SDK's FallbackProvider asks every fallback for the SAME
// (primary) model name, so without this a configured fallback model would never
// be used — the fallback would just retry the primary's model name elsewhere.
type fixedModelProvider struct {
	inner agents.ModelProvider
	model string
}

func (f fixedModelProvider) GetModel(string) (agents.Model, error) {
	return f.inner.GetModel(f.model)
}

// wrapFallbackProvider chains one fixed-model provider per decoded fallback
// entry behind primary. The entries are decoded up front (DecodeAgentSpec), so
// this is pure construction — callers gate it on len(entries) > 0.
func wrapFallbackProvider(primary agents.ModelProvider, entries []fallbackEntry, proxyClient *http.Client) agents.ModelProvider {
	var fallbacks []agents.ModelProvider
	for _, e := range entries {
		var opts []option.RequestOption
		if e.APIKey != "" {
			opts = append(opts, option.WithAPIKey(e.APIKey))
		}
		if e.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(e.BaseURL))
		}
		if proxyClient != nil {
			opts = append(opts, option.WithHTTPClient(proxyClient))
		}
		var fp agents.ModelProvider = openaiProvider.NewProvider(opts...)
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
	if err != nil || len(routes) == 0 {
		return fallback
	}
	proxyClient := ProxyHTTPClient(ctx, deps.Settings)
	routeMap := make(map[string]agents.ModelProvider, len(routes))
	for _, r := range routes {
		var opts []option.RequestOption
		if r.APIKey != "" {
			opts = append(opts, option.WithAPIKey(r.APIKey))
		}
		if r.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(r.BaseURL))
		}
		if proxyClient != nil {
			opts = append(opts, option.WithHTTPClient(proxyClient))
		}
		routeMap[r.Prefix] = openaiProvider.NewProvider(opts...)
	}
	router := agents.NewRouterProvider(routeMap)
	if fallback != nil {
		router.WithFallback(fallback)
	}
	return router
}
