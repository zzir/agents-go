// Package server wires up the gin HTTP engine, route registration, static-file serving, and the WebSocket upgrade.
package server

import (
	"github.com/gin-gonic/gin"
)

// Routes is the full set of handler funcs the server mounts; RegisterRoutes wires them to paths.
type Routes struct {
	SessionList     gin.HandlerFunc
	SessionCreate   gin.HandlerFunc
	SessionGet      gin.HandlerFunc
	SessionPatch    gin.HandlerFunc
	SessionDelete   gin.HandlerFunc
	SessionMessages gin.HandlerFunc
	SessionFork     gin.HandlerFunc
	WSHandler       WSHandlerFunc

	// Runs are started on a session and observed by run id (REST + SSE),
	// sharing the runner hub with the WebSocket transport.
	RunCreate gin.HandlerFunc
	RunGet    gin.HandlerFunc
	RunCancel gin.HandlerFunc
	RunEvents gin.HandlerFunc

	// Approvals are the human-in-the-loop decisions a paused run is waiting on.
	ApprovalListBySession gin.HandlerFunc
	ApprovalApprove       gin.HandlerFunc
	ApprovalReject        gin.HandlerFunc

	AgentList   gin.HandlerFunc
	AgentCreate gin.HandlerFunc
	AgentGet    gin.HandlerFunc
	AgentUpdate gin.HandlerFunc
	AgentDelete gin.HandlerFunc

	McpServerList          gin.HandlerFunc
	McpServerCreate        gin.HandlerFunc
	McpServerGet           gin.HandlerFunc
	McpServerUpdate        gin.HandlerFunc
	McpServerDelete        gin.HandlerFunc
	McpServerConnect       gin.HandlerFunc
	McpServerClearOAuth    gin.HandlerFunc
	McpServerTools         gin.HandlerFunc
	McpServerOAuthCallback gin.HandlerFunc

	MemoryList   gin.HandlerFunc
	MemoryCreate gin.HandlerFunc
	MemoryGet    gin.HandlerFunc
	MemoryUpdate gin.HandlerFunc
	MemoryDelete gin.HandlerFunc

	SettingList   gin.HandlerFunc
	SettingGet    gin.HandlerFunc
	SettingSet    gin.HandlerFunc
	SettingDelete gin.HandlerFunc

	// Skills are read-only resources; management (clone/sync/delete) operates
	// on whole repos under /skill-repos.
	SkillList       gin.HandlerFunc
	SkillGet        gin.HandlerFunc
	SkillRepoClone  gin.HandlerFunc
	SkillRepoSync   gin.HandlerFunc
	SkillRepoDelete gin.HandlerFunc

	ProviderRouteList   gin.HandlerFunc
	ProviderRouteCreate gin.HandlerFunc
	ProviderRouteGet    gin.HandlerFunc
	ProviderRouteUpdate gin.HandlerFunc
	ProviderRouteDelete gin.HandlerFunc

	GuardrailList   gin.HandlerFunc
	GuardrailCreate gin.HandlerFunc
	GuardrailGet    gin.HandlerFunc
	GuardrailUpdate gin.HandlerFunc
	GuardrailDelete gin.HandlerFunc

	SandboxList   gin.HandlerFunc
	SandboxCreate gin.HandlerFunc
	SandboxGet    gin.HandlerFunc
	SandboxUpdate gin.HandlerFunc
	SandboxDelete gin.HandlerFunc
	SandboxTest   gin.HandlerFunc

	TraceListBySession gin.HandlerFunc

	PlaygroundGenerate gin.HandlerFunc

	// ChatGPT OAuth is a per-agent capability mounted under /agents/:id/chatgpt.
	ChatGPTLogin    gin.HandlerFunc
	ChatGPTCallback gin.HandlerFunc
	ChatGPTStatus   gin.HandlerFunc
	ChatGPTLogout   gin.HandlerFunc
}

// RegisterRoutes mounts every handler in r under /api/v1 (canonical) and /api
// (deprecated alias kept for one release), plus the WebSocket endpoint.
func (s *Server) RegisterRoutes(r Routes) {
	registerAPIRoutes(s.Engine.Group("/api/v1"), r)
	registerAPIRoutes(s.Engine.Group("/api"), r)
	s.Engine.GET("/ws", HandleWSWithAuth(r.WSHandler, s.token))
}

// ServeOpenAPI mounts the OpenAPI document (auth-exempt) at
// /api/v1/openapi.yaml and the /api alias.
func (s *Server) ServeOpenAPI(spec []byte) {
	h := func(c *gin.Context) {
		c.Data(200, "application/yaml; charset=utf-8", spec)
	}
	s.Engine.GET("/api/v1/openapi.yaml", h)
	s.Engine.GET("/api/openapi.yaml", h)
}

func registerAPIRoutes(api *gin.RouterGroup, r Routes) {
	{
		sessions := api.Group("/sessions")
		sessions.GET("", r.SessionList)
		sessions.POST("", r.SessionCreate)
		sessions.GET("/:id", r.SessionGet)
		sessions.PATCH("/:id", r.SessionPatch)
		sessions.DELETE("/:id", r.SessionDelete)
		sessions.GET("/:id/messages", r.SessionMessages)
		sessions.POST("/:id/fork", r.SessionFork)
		sessions.GET("/:id/traces", r.TraceListBySession)
		sessions.POST("/:id/runs", r.RunCreate)
		sessions.GET("/:id/approvals", r.ApprovalListBySession)
	}
	{
		runs := api.Group("/runs")
		runs.GET("/:id", r.RunGet)
		runs.GET("/:id/events", r.RunEvents)
		runs.POST("/:id/cancel", r.RunCancel)
	}
	{
		approvals := api.Group("/approvals")
		approvals.POST("/:tool_call_id/approve", r.ApprovalApprove)
		approvals.POST("/:tool_call_id/reject", r.ApprovalReject)
	}
	{
		agents := api.Group("/agents")
		agents.GET("", r.AgentList)
		agents.POST("", r.AgentCreate)
		agents.GET("/:id", r.AgentGet)
		agents.PUT("/:id", r.AgentUpdate)
		agents.DELETE("/:id", r.AgentDelete)
		agents.POST("/:id/chatgpt/login", r.ChatGPTLogin)
		agents.POST("/:id/chatgpt/logout", r.ChatGPTLogout)
		agents.GET("/:id/chatgpt/status", r.ChatGPTStatus)
	}
	{
		mcpServers := api.Group("/mcp-servers")
		mcpServers.GET("", r.McpServerList)
		mcpServers.POST("", r.McpServerCreate)
		mcpServers.GET("/oauth/callback", r.McpServerOAuthCallback)
		mcpServers.GET("/:id", r.McpServerGet)
		mcpServers.PUT("/:id", r.McpServerUpdate)
		mcpServers.DELETE("/:id", r.McpServerDelete)
		mcpServers.POST("/:id/connect", r.McpServerConnect)
		mcpServers.DELETE("/:id/oauth-token", r.McpServerClearOAuth)
		mcpServers.GET("/:id/tools", r.McpServerTools)
	}
	{
		memories := api.Group("/memories")
		memories.GET("", r.MemoryList)
		memories.POST("", r.MemoryCreate)
		memories.GET("/:id", r.MemoryGet)
		memories.PUT("/:id", r.MemoryUpdate)
		memories.DELETE("/:id", r.MemoryDelete)
	}
	{
		settings := api.Group("/settings")
		settings.GET("", r.SettingList)
		settings.GET("/:key", r.SettingGet)
		settings.PUT("/:key", r.SettingSet)
		settings.DELETE("/:key", r.SettingDelete)
	}
	{
		skills := api.Group("/skills")
		skills.GET("", r.SkillList)
		skills.GET("/*path", r.SkillGet)

		repos := api.Group("/skill-repos")
		repos.POST("", r.SkillRepoClone)
		repos.POST("/:name/sync", r.SkillRepoSync)
		repos.DELETE("/:name", r.SkillRepoDelete)
	}
	{
		providerRoutes := api.Group("/provider-routes")
		providerRoutes.GET("", r.ProviderRouteList)
		providerRoutes.POST("", r.ProviderRouteCreate)
		providerRoutes.GET("/:id", r.ProviderRouteGet)
		providerRoutes.PUT("/:id", r.ProviderRouteUpdate)
		providerRoutes.DELETE("/:id", r.ProviderRouteDelete)
	}
	{
		guardrails := api.Group("/guardrails")
		guardrails.GET("", r.GuardrailList)
		guardrails.POST("", r.GuardrailCreate)
		guardrails.GET("/:id", r.GuardrailGet)
		guardrails.PUT("/:id", r.GuardrailUpdate)
		guardrails.DELETE("/:id", r.GuardrailDelete)
	}
	{
		sandboxes := api.Group("/sandboxes")
		sandboxes.GET("", r.SandboxList)
		sandboxes.POST("", r.SandboxCreate)
		sandboxes.GET("/:id", r.SandboxGet)
		sandboxes.PUT("/:id", r.SandboxUpdate)
		sandboxes.DELETE("/:id", r.SandboxDelete)
		sandboxes.POST("/:id/test", r.SandboxTest)
	}
	api.POST("/playground/generate", r.PlaygroundGenerate)
	api.GET("/chatgpt/oauth/callback", r.ChatGPTCallback)
}
