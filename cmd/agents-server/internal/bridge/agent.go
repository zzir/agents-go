// Package bridge adapts stored configuration into live agents-SDK constructs (agents, model providers, MCP servers, sandboxes, guardrails) and drives streamed runs over the WebSocket.
package bridge

import (
	"context"
	"encoding/json"
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
	"github.com/zzir/agents-go/tools/editor"
)

// AgentDeps holds all the dependencies needed to build a fully configured agent.
type AgentDeps struct {
	AgentConfigs   *store.AgentConfigStore
	McpServers     *store.McpServerStore
	SandboxConfigs *store.SandboxStore
	Memories       *store.MemoryStore
	Settings       *store.SettingStore
	ProviderRoutes *store.ProviderRouteStore
	Sessions       *store.SessionStore
	Traces         *store.TraceStore
	Guardrails     *GuardrailResolver
	McpManager     *McpManager
	SandboxManager *SandboxManager
	ChatGPTOAuth   *ChatGPTOAuth
	Workspace      string
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
	CompactionEnabled     bool
	CompactionThreshold   int
	CompactionWindow      int
	CompactionModel       string
	CompactionPrompt      string
}

// BuildFullAgent constructs an *agents.Agent from a config ID, loading all
// associated resources: provider, MCP tools, sandbox CodeTool, memory, and
// global settings (system_prompt). agentConfigID is required. sandboxID is
// optional — when set, only that sandbox is attached; when empty, all are.
func BuildFullAgent(ctx context.Context, deps *AgentDeps, agentConfigID, sandboxID string) (*BuildResult, error) {
	if agentConfigID == "" {
		return nil, fmt.Errorf("agent_config_id is required")
	}
	visited := make(map[string]bool)
	return buildAgentFromConfig(ctx, deps, agentConfigID, sandboxID, visited)
}

func buildAgentFromConfig(ctx context.Context, deps *AgentDeps, configID, sandboxID string, visited map[string]bool) (*BuildResult, error) {
	if visited[configID] {
		return nil, fmt.Errorf("cycle detected in handoff chain: %s", configID)
	}
	visited[configID] = true

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
		HandoffDescription:     ac.HandoffDescription,
		DisableToolChoiceReset: ac.DisableToolChoiceReset,
	}
	result.MaxTurns = ac.MaxTurns
	result.UsePreviousResponseID = ac.UsePreviousResponseID
	result.HandoffInputFilter = ac.HandoffInputFilter
	result.MaxToolConcurrency = ac.MaxToolConcurrency
	result.ToolNotFoundBehavior = ac.ToolNotFoundBehavior
	result.CompactionEnabled = ac.CompactionEnabled
	result.CompactionThreshold = ac.CompactionThreshold
	result.CompactionWindow = ac.CompactionWindow
	result.CompactionModel = ac.CompactionModel
	result.CompactionPrompt = ac.CompactionPrompt

	if ac.Instructions != "" {
		agent.Instructions = agents.StaticInstructions(ac.Instructions)
	}
	if ac.ModelSettings != "" {
		var ms agents.ModelSettings
		if err := json.Unmarshal([]byte(ac.ModelSettings), &ms); err == nil {
			agent.ModelSettings = &ms
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(ac.ModelSettings), &raw) == nil {
			if eb, ok := raw["extra_body"]; ok {
				var extraBody map[string]any
				if json.Unmarshal(eb, &extraBody) == nil && len(extraBody) > 0 {
					if agent.ModelSettings == nil {
						agent.ModelSettings = &agents.ModelSettings{}
					}
					agent.ModelSettings.ExtraBody = extraBody
				}
			}
		}
	}

	// ToolUseBehavior
	agent.ToolUseBehavior = agents.ParseToolUseBehavior(ac.ToolUseBehavior)

	// Guardrails
	if ac.InputGuardrails != "" && deps.Guardrails != nil {
		agent.InputGuardrails = deps.Guardrails.BuildInputGuardrails(ctx, ac.InputGuardrails)
	}
	if ac.OutputGuardrails != "" && deps.Guardrails != nil {
		agent.OutputGuardrails = deps.Guardrails.BuildOutputGuardrails(ctx, ac.OutputGuardrails)
	}

	// HITL tool approval
	if ac.ApproveTools != "" {
		var names []string
		if json.Unmarshal([]byte(ac.ApproveTools), &names) == nil && len(names) > 0 {
			agent.ApproveTools = names
		}
	}

	// Output schema (structured output)
	if ac.OutputSchema != "" {
		agent.OutputType = BuildOutputSchema(ac.OutputSchema)
	}

	// Stored prompt
	if ac.PromptID != "" {
		agent.Prompt = agents.StaticPrompt(agents.Prompt{
			ID:      ac.PromptID,
			Version: ac.PromptVersion,
		})
	}

	// Provider + retry/fallback decorators
	proxyClient := ProxyHTTPClient(ctx, deps.Settings)
	apiKey := ac.APIKey
	var chatgptCreds *ChatGPTCredentials
	if ac.AuthMode == "chatgpt_login" && deps.ChatGPTOAuth != nil {
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
		ac.APIKey = apiKey
		if chatgptCreds != nil && ac.BaseURL == "" {
			ac.BaseURL = ChatGPTBaseURL
		}
		provider := buildProviderFromConfig(ac, chatgptCreds, proxyClient)
		if ac.RetryEnabled {
			var policy agents.RetryPolicy
			if ac.RetryPolicy != "" {
				_ = json.Unmarshal([]byte(ac.RetryPolicy), &policy)
			}
			provider = agents.NewRetryProvider(provider, policy)
		}
		if ac.FallbackModels != "" {
			provider = wrapFallbackProvider(provider, ac.FallbackModels, proxyClient)
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

	// MCP servers
	if ac.ToolsJSON != "" {
		var mcpServerIDs []string
		if err := json.Unmarshal([]byte(ac.ToolsJSON), &mcpServerIDs); err == nil {
			for _, id := range mcpServerIDs {
				srv := deps.McpManager.Get(id)
				if srv != nil {
					agent.MCPServers = append(agent.MCPServers, srv)
				} else {
					log.Debug().Str("mcp_id", id).Msg("MCP server not connected, skipping")
				}
			}
		}
	}

	// Sandbox tools: "" = none, else = specific ID
	if sandboxID != "" {
		sbCfg, err := deps.SandboxConfigs.Get(ctx, sandboxID)
		if err == nil {
			tools, err := deps.SandboxManager.SandboxTools(sbCfg)
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

	// Editor tools
	if settingValue(ctx, deps.Settings, "enable_editor_tools") == "true" && deps.Workspace != "" {
		agent.Tools = append(agent.Tools, editor.NewTools(deps.Workspace)...)
	}

	// Skills — loaded from <root>/skills, matching where SkillHandler manages
	// them. Load and ReadFileTool must share this root so the relative paths in
	// the rendered index resolve correctly.
	if deps.Workspace != "" {
		skillsDir := filepath.Join(deps.Workspace, "skills")
		loadedSkills, err := skills.Load(skillsDir)
		if err == nil && len(loadedSkills) > 0 {
			agent.Instructions = agents.WrapInstructions(agent.Instructions, "", skills.RenderIndex(loadedSkills))
			agent.Tools = append(agent.Tools, skills.ReadFileTool(skillsDir))
		}
	}

	// Handoffs — recursive build
	if ac.HandoffsJSON != "" {
		var handoffIDs []string
		if err := json.Unmarshal([]byte(ac.HandoffsJSON), &handoffIDs); err == nil {
			for _, hID := range handoffIDs {
				hResult, err := buildAgentFromConfig(ctx, deps, hID, sandboxID, visited)
				if err != nil {
					log.Warn().Err(err).Str("handoff_id", hID).Msg("handoff agent build failed, skipping")
					continue
				}
				agent.Handoffs = append(agent.Handoffs, agents.HandoffTo(hResult.Agent))
			}
		}
	}

	result.Agent = agent
	return result, nil
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
	if ac.APIKey != "" {
		opts = append(opts, option.WithAPIKey(ac.APIKey))
	}
	if ac.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(ac.BaseURL))
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
						cleaned := make([]any, 0, len(input))
						for _, item := range input {
							if m, ok := item.(map[string]any); ok {
								delete(m, "id")
								if m["type"] == "item_reference" {
									continue
								}
							}
							cleaned = append(cleaned, item)
						}
						body["input"] = cleaned
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

func wrapFallbackProvider(primary agents.ModelProvider, fallbackJSON string, proxyClient *http.Client) agents.ModelProvider {
	var entries []fallbackEntry
	if json.Unmarshal([]byte(fallbackJSON), &entries) != nil || len(entries) == 0 {
		return primary
	}
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
		fallbacks = append(fallbacks, openaiProvider.NewProvider(opts...))
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
