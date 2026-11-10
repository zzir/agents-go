package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Handlers is the full set of REST handlers the server mounts; Register wires
// them to paths, so adding an endpoint is a handler method plus one line below.
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
	Workspaces *WorkspaceHandler
	Traces     *TraceHandler
	Playground *PlaygroundHandler
	Files      *AttachmentHandler
	// Server is the start-up configuration, served as read-only facts.
	Server ServerInfo
}

// wellFormedID answers 404 for an :id that is not an id: no row has one, and
// PostgreSQL would refuse it where SQLite finds nothing.
func wellFormedID(c *gin.Context) error {
	if id := c.Param("id"); id != "" && !store.IsID(id) {
		return errNotFound
	}
	return nil
}

// Hook is the webhook endpoint, mounted outside the authenticated API.
func (h Handlers) Hook() gin.HandlerFunc { return wrap(h.Triggers.Hook) }

// Register mounts every REST endpoint under api.
func (h Handlers) Register(api *gin.RouterGroup) {
	api.Use(wrap(wellFormedID))
	{
		auth := api.Group("/auth")
		// The guess budget on the credential-guess routes; the flow steps get
		// the looser one.
		guess, flow := server.AuthRateLimit(), server.FlowRateLimit()
		auth.POST("/login", guess, wrap(h.Auth.Login))
		auth.POST("/exchange", guess, wrap(h.Auth.Exchange))
		auth.GET("/oauth/:provider/start", flow, wrap(h.Auth.OAuthStart))
		auth.GET("/oauth/:provider/callback", flow, wrap(h.Auth.OAuthCallback))
		auth.GET("/config", wrap(h.Auth.Config))
		auth.GET("/check", wrap(h.Auth.Check))
		auth.GET("/me", wrap(h.Auth.Me))
		auth.POST("/logout", wrap(h.Auth.Logout))
		auth.GET("/tokens", wrap(h.Auth.ListTokens))
		auth.POST("/tokens", wrap(h.Auth.CreateToken))
		auth.DELETE("/tokens/:id", wrap(h.Auth.DeleteToken))
		auth.GET("/users", wrap(requireAdmin), wrap(h.Auth.ListUsers))
		auth.GET("/user-labels", wrap(requireAdmin), wrap(h.Auth.ListUserLabels))
		auth.PATCH("/users/:id", wrap(requireAdmin), wrap(h.Auth.PatchUser))
		auth.DELETE("/users/:id/tokens", wrap(requireAdmin), wrap(h.Auth.RevokeUserTokens))
		auth.GET("/audit", wrap(requireAdmin), wrap(h.Auth.ListAudit))
	}
	// Route-level authz — authz.go, decisions §5.29.
	admin := wrap(requireAdmin)
	{
		sessions := api.Group("/sessions")
		sessions.GET("", wrap(h.Sessions.List))
		sessions.POST("", wrap(h.Sessions.Create))
		// Delete and reassign are management: the owner or an admin deletes,
		// an admin reassigns — neither reads.
		sessions.DELETE("/:id", wrap(h.Sessions.Delete))
		sessions.PUT("/:id/owner", admin, wrap(h.Sessions.SetOwner))
		owned := sessions.Group("/:id", h.Authz.sessionGate())
		owned.GET("", wrap(h.Sessions.Get))
		owned.PATCH("", wrap(h.Sessions.Patch))
		owned.GET("/messages", wrap(h.Sessions.Messages))
		owned.GET("/context", wrap(h.Sessions.Context))
		owned.POST("/compact", wrap(h.Sessions.Compact))
		owned.POST("/fork", wrap(h.Sessions.Fork))
		owned.POST("/branch", wrap(h.Sessions.Branch))
		owned.GET("/traces", wrap(h.Traces.ListBySession))
		owned.GET("/traces/:span_id", wrap(h.Traces.GetBySpan))
		owned.POST("/runs", wrap(h.Runs.Create))
		owned.GET("/approvals", wrap(h.Approvals.ListBySession))
		owned.POST("/approvals/approve-all", wrap(h.Approvals.ApproveAll))
		owned.GET("/tasks", wrap(h.Tasks.ListBySession))
		owned.GET("/memory", wrap(h.Memories.ListSession))
		owned.GET("/memory/*key", wrap(h.Memories.ReadSession))
	}
	{
		runs := api.Group("/runs/:id", h.Authz.runGate())
		runs.GET("", wrap(h.Runs.Get))
		runs.GET("/events", wrap(h.Runs.Events))
		runs.POST("/cancel", wrap(h.Runs.Cancel))
		runs.POST("/inject", wrap(h.Runs.Inject))
	}
	{
		tasks := api.Group("/tasks")
		tasks.GET("", wrap(h.Tasks.List))
		task := tasks.Group("/:id", h.Authz.taskGate())
		task.POST("/stop", wrap(h.Tasks.Stop))
		task.POST("/retry", wrap(h.Tasks.Retry))
		task.POST("/dismiss", wrap(h.Tasks.Dismiss))

		// The caller's own inbox: scoped to the caller in the handler, no gate.
		api.GET("/approvals", wrap(h.Approvals.List))
		approvals := api.Group("/approvals/:tool_call_id", h.Authz.approvalGate())
		approvals.POST("/approve", wrap(h.Approvals.Approve))
		approvals.POST("/reject", wrap(h.Approvals.Reject))
	}
	{
		agents := api.Group("/agents")
		agents.GET("", wrap(h.Agents.List))
		agents.POST("", wrap(h.Agents.Create))
		agents.GET("/:id", wrap(h.Agents.Get))
		// The tool surface is assembled by BuildFullAgent, which lives with
		// the playground handler's deps — not a CRUD concern.
		agents.GET("/:id/tools", wrap(h.Playground.AgentTools))
		agents.PUT("/:id", wrap(h.Agents.Update))
		agents.DELETE("/:id", wrap(h.Agents.Delete))
		// Scope flips authorize in the handler: promote is admin-only, demote
		// is the admin's or the owner's (scopeChangeAllowed).
		agents.POST("/:id/scope", wrap(h.Agents.SetScope))
		agents.PUT("/:id/owner", admin, wrap(h.Agents.SetOwner))
	}
	{
		mcpServers := api.Group("/mcp-servers")
		mcpServers.GET("", wrap(h.McpServers.List))
		mcpServers.POST("", wrap(h.McpServers.Create))
		api.GET(mcpOAuthCallbackPath, wrap(h.McpServers.OAuthCallback))
		mcpServers.GET("/:id", wrap(h.McpServers.Get))
		mcpServers.PUT("/:id", wrap(h.McpServers.Update))
		mcpServers.DELETE("/:id", wrap(h.McpServers.Delete))
		mcpServers.POST("/:id/connect", wrap(h.McpServers.Connect))
		mcpServers.DELETE("/:id/oauth-token", wrap(h.McpServers.ClearOAuth))
		mcpServers.GET("/:id/tools", wrap(h.McpServers.Tools))
		mcpServers.POST("/:id/scope", wrap(h.McpServers.SetScope))
		mcpServers.PUT("/:id/owner", admin, wrap(h.McpServers.SetOwner))
	}
	{
		// Who writes which scope is store.MemoryPolicies, decided per row
		// inside the handler (invariant 64), so no route-level admin gate.
		memories := api.Group("/memories")
		memories.GET("", wrap(h.Memories.List))
		memories.POST("", wrap(h.Memories.Create))
		memories.GET("/:id", wrap(h.Memories.Get))
		memories.PUT("/:id", wrap(h.Memories.Update))
		memories.DELETE("/:id", wrap(h.Memories.Delete))
	}
	{
		settings := api.Group("/settings")
		settings.GET("", wrap(h.Settings.List))
		settings.GET("/:key", wrap(h.Settings.Get))
		settings.PUT("/:key", admin, wrap(h.Settings.Set))
		settings.DELETE("/:key", admin, wrap(h.Settings.Delete))
	}
	{
		skills := api.Group("/skills")
		skills.GET("", wrap(h.Skills.List))
		skills.GET("/:id", wrap(h.Skills.Get))
		skills.POST("", wrap(h.Skills.Create))
		skills.PUT("/:id", wrap(h.Skills.Update))
		skills.DELETE("/:id", wrap(h.Skills.Delete))
		skills.POST("/:id/scope", wrap(h.Skills.SetScope))
		skills.PUT("/:id/owner", admin, wrap(h.Skills.SetOwner))
		// Import and the repo-group scope flip are their own resources: gin
		// cannot mix a literal segment with the :id parameter above.
		api.POST("/skill-imports", wrap(h.Skills.Import))
		api.POST("/skill-repos/scope", wrap(h.Skills.SetRepoScope))
	}
	// The two registries a config UI renders from: provider machine facts
	// and the global settings table.
	api.GET("/provider-types", wrap(ProviderTypeList))
	api.GET("/setting-defs", wrap(SettingDefList))
	// The command line's decisions, as read-only facts for the UI.
	api.GET("/server", ServerInfoHandler(h.Server))
	{
		providers := api.Group("/providers")
		providers.GET("", wrap(h.Providers.List))
		providers.POST("", wrap(h.Providers.Create))
		providers.GET("/:id", wrap(h.Providers.Get))
		providers.PUT("/:id", wrap(h.Providers.Update))
		providers.DELETE("/:id", wrap(h.Providers.Delete))
		providers.POST("/:id/scope", wrap(h.Providers.SetScope))
		providers.GET("/:id/models", wrap(h.Providers.Models))
		providers.POST("/:id/test", wrap(h.Providers.Test))
		providers.PUT("/:id/owner", admin, wrap(h.Providers.SetOwner))
		// The ChatGPT login is the provider's, gated in the handler (editable).
		providers.POST("/:id/chatgpt/login", wrap(h.Providers.ChatGPTLogin))
		providers.POST("/:id/chatgpt/complete", wrap(h.Providers.ChatGPTComplete))
		providers.POST("/:id/chatgpt/logout", wrap(h.Providers.ChatGPTLogout))
	}
	{
		workflows := api.Group("/workflows")
		workflows.GET("", wrap(h.Workflows.List))
		workflows.POST("", wrap(h.Workflows.Create))
		workflows.GET("/:id", wrap(h.Workflows.Get))
		workflows.PUT("/:id", wrap(h.Workflows.Update))
		workflows.DELETE("/:id", wrap(h.Workflows.Delete))
		workflows.POST("/:id/scope", wrap(h.Workflows.SetScope))
		workflows.PUT("/:id/owner", admin, wrap(h.Workflows.SetOwner))
		// Running one is a member's act, into a session they own (checked in
		// the handler — the session id rides the body).
		workflows.POST("/:id/runs", wrap(h.Workflows.Run))
		h.registerTriggers(api)
	}
	{
		// Workspaces are personal: the admin surface (?all=true listing, foreign
		// delete) is checked in the handlers, so no route-level gate here.
		workspaces := api.Group("/workspaces")
		workspaces.GET("", wrap(h.Workspaces.List))
		workspaces.POST("", wrap(h.Workspaces.Create))
		workspaces.GET("/:id", wrap(h.Workspaces.Get))
		workspaces.PUT("/:id", wrap(h.Workspaces.Update))
		workspaces.DELETE("/:id", wrap(h.Workspaces.Delete))
		workspaces.GET("/:id/export", wrap(h.Workspaces.Export))
		workspaces.GET("/:id/host", wrap(h.Workspaces.SandboxHost))
		workspaces.GET("/:id/sandbox", wrap(h.Workspaces.SandboxStatus))
		workspaces.POST("/:id/sandbox/start", wrap(h.Workspaces.SandboxStart))
		workspaces.POST("/:id/sandbox/stop", wrap(h.Workspaces.SandboxStop))
		workspaces.POST("/:id/sandbox/rebuild", wrap(h.Workspaces.RebuildContainer))
	}
	{
		guardrails := api.Group("/guardrails")
		guardrails.GET("", wrap(h.Guardrails.List))
		guardrails.POST("", admin, wrap(h.Guardrails.Create))
		guardrails.GET("/:id", wrap(h.Guardrails.Get))
		guardrails.PUT("/:id", admin, wrap(h.Guardrails.Update))
		guardrails.DELETE("/:id", admin, wrap(h.Guardrails.Delete))
	}
	{
		sbs := api.Group("/sandboxes")
		sbs.GET("", wrap(h.Sandboxes.List))
		sbs.POST("", admin, wrap(h.Sandboxes.Create))
		sbs.GET("/:id", wrap(h.Sandboxes.Get))
		sbs.PUT("/:id", admin, wrap(h.Sandboxes.Update))
		sbs.DELETE("/:id", admin, wrap(h.Sandboxes.Delete))
		sbs.POST("/:id/test", admin, wrap(h.Sandboxes.Test))
	}
	api.POST("/playground/generate", wrap(h.Playground.Generate))

	{
		atts := api.Group("/attachments")
		atts.GET("/config", wrap(h.Files.Config))
		atts.POST("", wrap(h.Files.Upload))
		// The storage section saves, tests and clears as one group (invariant
		// 58). Static routes registered before the :id param so gin matches them first.
		atts.PUT("/storage", wrap(requireAdmin), wrap(h.Files.SaveStorage))
		atts.POST("/storage/test", wrap(requireAdmin), wrap(h.Files.TestStorage))
		atts.DELETE("/:id", wrap(h.Files.Delete))
	}
}

// registerTriggers mounts the trigger endpoints (the webhook itself is mounted
// by the server, outside the API prefix).
func (h Handlers) registerTriggers(api *gin.RouterGroup) {
	// A trigger is as private as the session it fires into: the :id subtree
	// is gated on owning that session.
	triggers := api.Group("/triggers")
	triggers.GET("", wrap(h.Triggers.List))
	triggers.POST("", wrap(h.Triggers.Create))
	one := triggers.Group("/:id", h.Authz.triggerGate())
	one.GET("", wrap(h.Triggers.Get))
	one.PUT("", wrap(h.Triggers.Update))
	one.DELETE("", wrap(h.Triggers.Delete))
	one.POST("/fire", wrap(h.Triggers.Fire))
	one.POST("/rotate-secret", wrap(h.Triggers.RotateSecret))
}
