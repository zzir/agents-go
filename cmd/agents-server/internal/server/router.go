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
	SessionUpdate   gin.HandlerFunc
	SessionDelete   gin.HandlerFunc
	SessionMessages gin.HandlerFunc
	WSHandler       WSHandlerFunc

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
	McpServerDisconnect    gin.HandlerFunc
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

	SkillList   gin.HandlerFunc
	SkillGet    gin.HandlerFunc
	SkillClone  gin.HandlerFunc
	SkillUpdate gin.HandlerFunc
	SkillDelete gin.HandlerFunc

	FileList gin.HandlerFunc
	FileRead gin.HandlerFunc

	ProviderRouteList   gin.HandlerFunc
	ProviderRouteCreate gin.HandlerFunc
	ProviderRouteUpdate gin.HandlerFunc
	ProviderRouteDelete gin.HandlerFunc

	GuardrailList gin.HandlerFunc

	SandboxList   gin.HandlerFunc
	SandboxCreate gin.HandlerFunc
	SandboxGet    gin.HandlerFunc
	SandboxUpdate gin.HandlerFunc
	SandboxDelete gin.HandlerFunc
	SandboxExec   gin.HandlerFunc

	TraceListBySession gin.HandlerFunc

	ChatGPTLogin    gin.HandlerFunc
	ChatGPTCallback gin.HandlerFunc
	ChatGPTStatus   gin.HandlerFunc
	ChatGPTLogout   gin.HandlerFunc
}

// RegisterRoutes mounts every handler in r onto the server's gin engine at its API path.
func (s *Server) RegisterRoutes(r Routes) {
	api := s.Engine.Group("/api")
	{
		sessions := api.Group("/sessions")
		sessions.GET("", r.SessionList)
		sessions.POST("", r.SessionCreate)
		sessions.GET("/:id", r.SessionGet)
		sessions.PUT("/:id", r.SessionUpdate)
		sessions.DELETE("/:id", r.SessionDelete)
		sessions.GET("/:id/messages", r.SessionMessages)
		sessions.GET("/:id/traces", r.TraceListBySession)
	}
	{
		agents := api.Group("/agents")
		agents.GET("", r.AgentList)
		agents.POST("", r.AgentCreate)
		agents.GET("/:id", r.AgentGet)
		agents.PUT("/:id", r.AgentUpdate)
		agents.DELETE("/:id", r.AgentDelete)
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
		mcpServers.POST("/:id/disconnect", r.McpServerDisconnect)
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
		skills.POST("/clone", r.SkillClone)
		skills.PUT("/:name", r.SkillUpdate)
		skills.DELETE("/:name", r.SkillDelete)
		skills.GET("/*path", r.SkillGet)
	}
	{
		providerRoutes := api.Group("/provider-routes")
		providerRoutes.GET("", r.ProviderRouteList)
		providerRoutes.POST("", r.ProviderRouteCreate)
		providerRoutes.PUT("/:id", r.ProviderRouteUpdate)
		providerRoutes.DELETE("/:id", r.ProviderRouteDelete)
	}
	api.GET("/guardrails", r.GuardrailList)
	api.GET("/files", r.FileList)
	api.GET("/files/*path", r.FileRead)
	{
		sandboxes := api.Group("/sandboxes")
		sandboxes.GET("", r.SandboxList)
		sandboxes.POST("", r.SandboxCreate)
		sandboxes.GET("/:id", r.SandboxGet)
		sandboxes.PUT("/:id", r.SandboxUpdate)
		sandboxes.DELETE("/:id", r.SandboxDelete)
		sandboxes.POST("/:id/exec", r.SandboxExec)
	}
	{
		chatgpt := api.Group("/chatgpt")
		chatgpt.POST("/login", r.ChatGPTLogin)
		chatgpt.GET("/oauth/callback", r.ChatGPTCallback)
		chatgpt.GET("/status", r.ChatGPTStatus)
		chatgpt.POST("/logout", r.ChatGPTLogout)
	}
	s.Engine.GET("/ws", HandleWSWithAuth(r.WSHandler, s.token))
}
