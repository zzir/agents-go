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
	Authz      AuthzDeps
	Auth       *AuthHandler
	Sessions   *SessionHandler
	Runs       *RunHandler
	Approvals  *ApprovalHandler
	Tasks      *TaskHandler
	Agents     *AgentConfigHandler
	McpServers *McpServerHandler
	Memories   *MemoryHandler
	Settings   *SettingHandler
	Skills     *SkillHandler
	Providers  *ProviderHandler
	Workflows  *WorkflowHandler
	Triggers   *TriggerHandler
	Guardrails *GuardrailHandler
	Sandboxes  *SandboxHandler
	Projects   *ProjectHandler
	Traces     *TraceHandler
	Playground *PlaygroundHandler
	ChatGPT    *ChatGPTOAuthHandler
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
		auth.PATCH("/users/:id", adminOnly(), h.Auth.PatchUser)
		auth.DELETE("/users/:id/tokens", adminOnly(), h.Auth.RevokeUserTokens)
		auth.GET("/audit", adminOnly(), h.Auth.ListAudit)
	}
	// Three rules shape everything below (authz.go, spec §5.29): a session's
	// content is its owner's alone — the :id subtrees are gated on ownership,
	// and a foreign id reads as absent. SCOPED configuration (agents,
	// providers, MCP servers, skills, workflows) is per-row: global rows are
	// every member's to read and admins' to write, private rows their
	// owner's; the gates live in the handlers, and the scope-change routes
	// are admin-only. HOST configuration (sandboxes, settings, guardrails,
	// memories) stays read-everyone, write-admin.
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
		agents.POST("", h.Agents.Create)
		agents.GET("/:id", h.Agents.Get)
		// The tool surface is assembled by BuildFullAgent, which lives with
		// the playground handler's deps — not a CRUD concern.
		agents.GET("/:id/tools", h.Playground.AgentTools)
		agents.PUT("/:id", h.Agents.Update)
		agents.DELETE("/:id", h.Agents.Delete)
		agents.POST("/:id/scope", admin, h.Agents.SetScope)
	}
	{
		mcpServers := api.Group("/mcp-servers")
		mcpServers.GET("", h.McpServers.List)
		mcpServers.POST("", h.McpServers.Create)
		api.GET(mcpOAuthCallbackPath, h.McpServers.OAuthCallback)
		mcpServers.GET("/:id", h.McpServers.Get)
		mcpServers.PUT("/:id", h.McpServers.Update)
		mcpServers.DELETE("/:id", h.McpServers.Delete)
		mcpServers.POST("/:id/connect", h.McpServers.Connect)
		mcpServers.DELETE("/:id/oauth-token", h.McpServers.ClearOAuth)
		mcpServers.GET("/:id/tools", h.McpServers.Tools)
		mcpServers.POST("/:id/scope", admin, h.McpServers.SetScope)
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
		skills := api.Group("/skills")
		skills.GET("", h.Skills.List)
		skills.GET("/:id", h.Skills.Get)
		skills.POST("", h.Skills.Create)
		skills.PUT("/:id", h.Skills.Update)
		skills.DELETE("/:id", h.Skills.Delete)
		skills.POST("/:id/scope", admin, h.Skills.SetScope)
		// Import is its own resource, not a /skills subpath: gin cannot mix a
		// literal segment with the :id parameter above.
		api.POST("/skill-imports", h.Skills.Import)
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
		providers.POST("", h.Providers.Create)
		providers.GET("/:id", h.Providers.Get)
		providers.PUT("/:id", h.Providers.Update)
		providers.DELETE("/:id", h.Providers.Delete)
		providers.POST("/:id/scope", admin, h.Providers.SetScope)
		// The OAuth flow belongs to the endpoint, not to any one agent —
		// signing a private provider into ChatGPT is its owner's act.
		providers.POST("/:id/chatgpt/login", h.ChatGPT.Login)
		providers.POST("/:id/chatgpt/logout", h.ChatGPT.Logout)
		providers.GET("/:id/chatgpt/status", h.ChatGPT.Status)
	}
	{
		workflows := api.Group("/workflows")
		workflows.GET("", h.Workflows.List)
		workflows.POST("", h.Workflows.Create)
		workflows.GET("/:id", h.Workflows.Get)
		workflows.PUT("/:id", h.Workflows.Update)
		workflows.DELETE("/:id", h.Workflows.Delete)
		workflows.POST("/:id/scope", admin, h.Workflows.SetScope)
		// Running one is a member's act, into a session they own (checked in
		// the handler — the session id rides the body).
		workflows.POST("/:id/runs", h.Workflows.Run)
		h.registerTriggers(api)
	}
	{
		// Projects are personal working trees: every member manages their own
		// (the handlers scope by owner), so no admin gate here.
		projects := api.Group("/projects")
		projects.GET("", h.Projects.List)
		projects.POST("", h.Projects.Create)
		projects.DELETE("/:id", h.Projects.Delete)
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
		sandboxes.GET("/:id/containers", admin, h.Sandboxes.Containers)
		sandboxes.POST("/:id/containers/:name/stop", admin, h.Sandboxes.StopContainer)
		sandboxes.DELETE("/:id/containers/:name", admin, h.Sandboxes.RemoveContainer)
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
