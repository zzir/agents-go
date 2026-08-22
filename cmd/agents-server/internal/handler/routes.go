package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// Handlers is the full set of REST handlers the server mounts; Register wires
// them to paths. Registration lives here, next to the handlers, so adding an
// endpoint means a handler method plus one line below — not a field in a
// per-endpoint forwarding struct kept in sync across packages.
type Handlers struct {
	// Authz resolves ownership for the route gates (see authz.go).
	Authz          AuthzDeps
	Auth           *AuthHandler
	Sessions       *SessionHandler
	Runs           *RunHandler
	Approvals      *ApprovalHandler
	Tasks          *TaskHandler
	Agents         *AgentConfigHandler
	McpServers     *McpServerHandler
	Memories       *MemoryHandler
	Settings       *SettingHandler
	Skills         *SkillHandler
	Providers      *ProviderHandler
	Workflows      *WorkflowHandler
	Triggers       *TriggerHandler
	ProviderRoutes *ProviderRouteHandler
	Guardrails     *GuardrailHandler
	Sandboxes      *SandboxHandler
	Traces         *TraceHandler
	Playground     *PlaygroundHandler
	ChatGPT        *ChatGPTOAuthHandler
	// Server is the start-up configuration, served as read-only facts.
	Server ServerInfo
}

// Register mounts every REST endpoint under api.
func (h Handlers) Register(api *gin.RouterGroup) {
	{
		auth := api.Group("/auth")
		// The guess budget sits on the two routes where every request IS a
		// credential guess; the flow steps get the looser budget; config is a
		// static fact. Authenticated routes (check included) draw on the
		// failure budget in server.TokenAuth, which a valid bearer never spends.
		guess, flow := server.AuthRateLimit(), server.FlowRateLimit()
		auth.POST("/login", guess, h.Auth.Login)
		auth.POST("/exchange", guess, h.Auth.Exchange)
		auth.GET("/oauth/:provider/start", flow, h.Auth.OAuthStart)
		auth.GET("/oauth/:provider/callback", flow, h.Auth.OAuthCallback)
		auth.GET("/config", h.Auth.Config)
		auth.GET("/check", h.Auth.Check)
		auth.GET("/me", h.Auth.Me)
		auth.POST("/logout", h.Auth.Logout)
		auth.GET("/tokens", h.Auth.ListTokens)
		auth.POST("/tokens", h.Auth.CreateToken)
		auth.DELETE("/tokens/:id", h.Auth.DeleteToken)
		auth.GET("/users", adminOnly(), h.Auth.ListUsers)
		auth.PUT("/users/:id/role", adminOnly(), h.Auth.SetUserRole)
		auth.GET("/audit", adminOnly(), h.Auth.ListAudit)
	}
	// Two rules shape everything below (authz.go): a session's content is its
	// owner's alone — the :id subtrees are gated on ownership, and a foreign
	// id reads as absent; shared configuration is read by every member and
	// written by admins only.
	admin := adminOnly()
	{
		sessions := api.Group("/sessions")
		sessions.GET("", h.Sessions.List)
		sessions.POST("", h.Sessions.Create)
		// Delete and reassign are management: the owner or an admin deletes,
		// an admin reassigns — neither reads.
		sessions.DELETE("/:id", h.Sessions.Delete)
		sessions.PUT("/:id/owner", admin, h.Sessions.SetOwner)
		owned := sessions.Group("/:id", h.Authz.sessionGate())
		owned.GET("", h.Sessions.Get)
		owned.PATCH("", h.Sessions.Patch)
		owned.GET("/messages", h.Sessions.Messages)
		owned.GET("/context", h.Sessions.Context)
		owned.POST("/compact", h.Sessions.Compact)
		owned.POST("/fork", h.Sessions.Fork)
		owned.POST("/branch", h.Sessions.Branch)
		owned.GET("/traces", h.Traces.ListBySession)
		owned.GET("/traces/:span_id", h.Traces.GetBySpan)
		owned.GET("/runs", h.Sessions.Runs)
		owned.POST("/runs", h.Runs.Create)
		owned.GET("/approvals", h.Approvals.ListBySession)
		owned.GET("/tasks", h.Tasks.ListBySession)
	}
	{
		runs := api.Group("/runs/:id", h.Authz.runGate())
		runs.GET("", h.Runs.Get)
		runs.GET("/events", h.Runs.Events)
		runs.POST("/cancel", h.Runs.Cancel)
	}
	{
		tasks := api.Group("/tasks")
		tasks.GET("", h.Tasks.List)
		task := tasks.Group("/:id", h.Authz.taskGate())
		task.POST("/stop", h.Tasks.Stop)
		task.POST("/retry", h.Tasks.Retry)
		task.POST("/dismiss", h.Tasks.Dismiss)

		approvals := api.Group("/approvals/:tool_call_id", h.Authz.approvalGate())
		approvals.POST("/approve", h.Approvals.Approve)
		approvals.POST("/reject", h.Approvals.Reject)
	}
	{
		agents := api.Group("/agents")
		agents.GET("", h.Agents.List)
		agents.POST("", admin, h.Agents.Create)
		agents.GET("/:id", h.Agents.Get)
		// The tool surface is assembled by BuildFullAgent, which lives with
		// the playground handler's deps — not a CRUD concern.
		agents.GET("/:id/tools", h.Playground.AgentTools)
		agents.PUT("/:id", admin, h.Agents.Update)
		agents.DELETE("/:id", admin, h.Agents.Delete)
	}
	{
		mcpServers := api.Group("/mcp-servers")
		mcpServers.GET("", h.McpServers.List)
		mcpServers.POST("", admin, h.McpServers.Create)
		mcpServers.GET("/oauth/callback", h.McpServers.OAuthCallback)
		mcpServers.GET("/:id", h.McpServers.Get)
		mcpServers.PUT("/:id", admin, h.McpServers.Update)
		mcpServers.DELETE("/:id", admin, h.McpServers.Delete)
		mcpServers.POST("/:id/connect", admin, h.McpServers.Connect)
		mcpServers.DELETE("/:id/oauth-token", admin, h.McpServers.ClearOAuth)
		mcpServers.GET("/:id/tools", h.McpServers.Tools)
	}
	{
		memories := api.Group("/memories")
		memories.GET("", h.Memories.List)
		memories.POST("", admin, h.Memories.Create)
		memories.GET("/:id", h.Memories.Get)
		memories.PUT("/:id", admin, h.Memories.Update)
		memories.DELETE("/:id", admin, h.Memories.Delete)
	}
	{
		settings := api.Group("/settings")
		settings.GET("", h.Settings.List)
		settings.GET("/:key", h.Settings.Get)
		settings.PUT("/:key", admin, h.Settings.Set)
		settings.DELETE("/:key", admin, h.Settings.Delete)
	}
	{
		// Skills are read-only resources; management (clone/sync/delete)
		// operates on whole repos under /skill-repos.
		skills := api.Group("/skills")
		skills.GET("", h.Skills.List)
		skills.GET("/*path", h.Skills.Get)

		repos := api.Group("/skill-repos", admin)
		repos.POST("", h.Skills.Clone)
		repos.POST("/:name/sync", h.Skills.Sync)
		repos.DELETE("/:name", h.Skills.Delete)
	}
	// The two registries a config UI renders from, so a panel never keeps its
	// own copy of what the server accepts: provider machine facts (types, auth
	// modes, unsupported features) and the global settings table.
	api.GET("/provider-types", ProviderTypeList)
	api.GET("/setting-defs", SettingDefList)
	// What the command line decided, so the UI can show the rules it is
	// subject to instead of only their refusals.
	api.GET("/server", ServerInfoHandler(h.Server))
	{
		providers := api.Group("/providers")
		providers.GET("", h.Providers.List)
		providers.POST("", admin, h.Providers.Create)
		providers.GET("/:id", h.Providers.Get)
		providers.PUT("/:id", admin, h.Providers.Update)
		providers.DELETE("/:id", admin, h.Providers.Delete)
		// The OAuth flow belongs to the endpoint, not to any one agent.
		providers.POST("/:id/chatgpt/login", admin, h.ChatGPT.Login)
		providers.POST("/:id/chatgpt/logout", admin, h.ChatGPT.Logout)
		providers.GET("/:id/chatgpt/status", h.ChatGPT.Status)

		providerRoutes := api.Group("/provider-routes")
		providerRoutes.GET("", h.ProviderRoutes.List)
		providerRoutes.POST("", admin, h.ProviderRoutes.Create)
		providerRoutes.GET("/:id", h.ProviderRoutes.Get)
		providerRoutes.PUT("/:id", admin, h.ProviderRoutes.Update)
		providerRoutes.DELETE("/:id", admin, h.ProviderRoutes.Delete)
	}
	{
		workflows := api.Group("/workflows")
		workflows.GET("", h.Workflows.List)
		workflows.POST("", admin, h.Workflows.Create)
		workflows.GET("/:id", h.Workflows.Get)
		workflows.PUT("/:id", admin, h.Workflows.Update)
		workflows.DELETE("/:id", admin, h.Workflows.Delete)
		// Running one is a member's act, into a session they own (checked in
		// the handler — the session id rides the body).
		workflows.POST("/:id/runs", h.Workflows.Run)
		h.registerTriggers(api)
	}
	{
		guardrails := api.Group("/guardrails")
		guardrails.GET("", h.Guardrails.List)
		guardrails.POST("", admin, h.Guardrails.Create)
		guardrails.GET("/:id", h.Guardrails.Get)
		guardrails.PUT("/:id", admin, h.Guardrails.Update)
		guardrails.DELETE("/:id", admin, h.Guardrails.Delete)
	}
	{
		sandboxes := api.Group("/sandboxes")
		sandboxes.GET("", h.Sandboxes.List)
		sandboxes.POST("", admin, h.Sandboxes.Create)
		sandboxes.GET("/:id", h.Sandboxes.Get)
		sandboxes.PUT("/:id", admin, h.Sandboxes.Update)
		sandboxes.DELETE("/:id", admin, h.Sandboxes.Delete)
		sandboxes.POST("/:id/test", admin, h.Sandboxes.Test)
	}
	api.POST("/playground/generate", h.Playground.Generate)
}

// registerTriggers mounts the trigger endpoints (the webhook itself is mounted
// by the server, outside the API prefix).
func (h Handlers) registerTriggers(api *gin.RouterGroup) {
	// A trigger is as private as the session it fires into: the list is the
	// caller's, Create checks the session (in bind), and the :id subtree is
	// gated on owning that session.
	triggers := api.Group("/triggers")
	triggers.GET("", h.Triggers.List)
	triggers.POST("", h.Triggers.Create)
	one := triggers.Group("/:id", h.Authz.triggerGate())
	one.GET("", h.Triggers.Get)
	one.PUT("", h.Triggers.Update)
	one.DELETE("", h.Triggers.Delete)
	one.POST("/fire", h.Triggers.Fire)
	one.POST("/rotate-secret", h.Triggers.RotateSecret)
}
