package handler

import (
	"github.com/gin-gonic/gin"
)

// Handlers is the full set of REST handlers the server mounts; Register wires
// them to paths. Registration lives here, next to the handlers, so adding an
// endpoint means a handler method plus one line below — not a field in a
// per-endpoint forwarding struct kept in sync across packages.
type Handlers struct {
	Sessions       *SessionHandler
	Runs           *RunHandler
	Approvals      *ApprovalHandler
	Tasks          *TaskHandler
	Agents         *AgentConfigHandler
	McpServers     *McpServerHandler
	Memories       *MemoryHandler
	Settings       *SettingHandler
	Skills         *SkillHandler
	ProviderRoutes *ProviderRouteHandler
	Guardrails     *GuardrailHandler
	Sandboxes      *SandboxHandler
	Traces         *TraceHandler
	Playground     *PlaygroundHandler
	ChatGPT        *ChatGPTOAuthHandler
}

// Register mounts every REST endpoint under api.
func (h Handlers) Register(api *gin.RouterGroup) {
	{
		sessions := api.Group("/sessions")
		sessions.GET("", h.Sessions.List)
		sessions.POST("", h.Sessions.Create)
		sessions.GET("/:id", h.Sessions.Get)
		sessions.PATCH("/:id", h.Sessions.Patch)
		sessions.DELETE("/:id", h.Sessions.Delete)
		sessions.GET("/:id/messages", h.Sessions.Messages)
		sessions.POST("/:id/fork", h.Sessions.Fork)
		sessions.POST("/:id/branch", h.Sessions.Branch)
		sessions.GET("/:id/traces", h.Traces.ListBySession)
		sessions.POST("/:id/runs", h.Runs.Create)
		sessions.GET("/:id/approvals", h.Approvals.ListBySession)
		sessions.GET("/:id/tasks", h.Tasks.ListBySession)
	}
	{
		runs := api.Group("/runs")
		runs.GET("/:id", h.Runs.Get)
		runs.GET("/:id/events", h.Runs.Events)
		runs.POST("/:id/cancel", h.Runs.Cancel)
	}
	{
		tasks := api.Group("/tasks")
		tasks.POST("/:id/stop", h.Tasks.Stop)
		tasks.POST("/:id/retry", h.Tasks.Retry)

		approvals := api.Group("/approvals")
		approvals.POST("/:tool_call_id/approve", h.Approvals.Approve)
		approvals.POST("/:tool_call_id/reject", h.Approvals.Reject)
	}
	{
		agents := api.Group("/agents")
		agents.GET("", h.Agents.List)
		agents.POST("", h.Agents.Create)
		agents.GET("/:id", h.Agents.Get)
		// The tool surface is assembled by BuildFullAgent, which lives with
		// the playground handler's deps — not a CRUD concern.
		agents.GET("/:id/tools", h.Playground.AgentTools)
		agents.PUT("/:id", h.Agents.Update)
		agents.DELETE("/:id", h.Agents.Delete)
		agents.POST("/:id/chatgpt/login", h.ChatGPT.Login)
		agents.POST("/:id/chatgpt/logout", h.ChatGPT.Logout)
		agents.GET("/:id/chatgpt/status", h.ChatGPT.Status)
	}
	{
		mcpServers := api.Group("/mcp-servers")
		mcpServers.GET("", h.McpServers.List)
		mcpServers.POST("", h.McpServers.Create)
		mcpServers.GET("/oauth/callback", h.McpServers.OAuthCallback)
		mcpServers.GET("/:id", h.McpServers.Get)
		mcpServers.PUT("/:id", h.McpServers.Update)
		mcpServers.DELETE("/:id", h.McpServers.Delete)
		mcpServers.POST("/:id/connect", h.McpServers.Connect)
		mcpServers.DELETE("/:id/oauth-token", h.McpServers.ClearOAuth)
		mcpServers.GET("/:id/tools", h.McpServers.Tools)
	}
	{
		memories := api.Group("/memories")
		memories.GET("", h.Memories.List)
		memories.POST("", h.Memories.Create)
		memories.GET("/:id", h.Memories.Get)
		memories.PUT("/:id", h.Memories.Update)
		memories.DELETE("/:id", h.Memories.Delete)
	}
	{
		settings := api.Group("/settings")
		settings.GET("", h.Settings.List)
		settings.GET("/:key", h.Settings.Get)
		settings.PUT("/:key", h.Settings.Set)
		settings.DELETE("/:key", h.Settings.Delete)
	}
	{
		// Skills are read-only resources; management (clone/sync/delete)
		// operates on whole repos under /skill-repos.
		skills := api.Group("/skills")
		skills.GET("", h.Skills.List)
		skills.GET("/*path", h.Skills.Get)

		repos := api.Group("/skill-repos")
		repos.POST("", h.Skills.Clone)
		repos.POST("/:name/sync", h.Skills.Sync)
		repos.DELETE("/:name", h.Skills.Delete)
	}
	// The provider registry's machine facts (types, auth modes, unsupported
	// features), so config UIs stay in sync with it.
	api.GET("/provider-types", ProviderTypeList)
	{
		providerRoutes := api.Group("/provider-routes")
		providerRoutes.GET("", h.ProviderRoutes.List)
		providerRoutes.POST("", h.ProviderRoutes.Create)
		providerRoutes.GET("/:id", h.ProviderRoutes.Get)
		providerRoutes.PUT("/:id", h.ProviderRoutes.Update)
		providerRoutes.DELETE("/:id", h.ProviderRoutes.Delete)
	}
	{
		guardrails := api.Group("/guardrails")
		guardrails.GET("", h.Guardrails.List)
		guardrails.POST("", h.Guardrails.Create)
		guardrails.GET("/:id", h.Guardrails.Get)
		guardrails.PUT("/:id", h.Guardrails.Update)
		guardrails.DELETE("/:id", h.Guardrails.Delete)
	}
	{
		sandboxes := api.Group("/sandboxes")
		sandboxes.GET("", h.Sandboxes.List)
		sandboxes.POST("", h.Sandboxes.Create)
		sandboxes.GET("/:id", h.Sandboxes.Get)
		sandboxes.PUT("/:id", h.Sandboxes.Update)
		sandboxes.DELETE("/:id", h.Sandboxes.Delete)
		sandboxes.POST("/:id/test", h.Sandboxes.Test)
	}
	api.POST("/playground/generate", h.Playground.Generate)
}
