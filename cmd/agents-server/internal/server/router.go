// Package server wires up the gin HTTP engine, static-file serving, and the
// WebSocket upgrade. REST route registration lives with the handlers
// (handler.Handlers.Register); the server only decides where the API is
// mounted.
package server

import (
	"github.com/gin-gonic/gin"
)

// RegisterAPI mounts register under /api/v1 (canonical) and /api (deprecated
// alias kept for one release).
func (s *Server) RegisterAPI(register func(*gin.RouterGroup)) {
	register(s.Engine.Group("/api/v1"))
	register(s.Engine.Group("/api"))
}

// RegisterWS mounts the WebSocket endpoints with token auth: /ws for run
// events, /ws/terminal for one interactive sandbox terminal per connection.
func (s *Server) RegisterWS(ws, terminal WSHandlerFunc) {
	s.Engine.GET("/ws", HandleWSWithAuth(ws, s.token))
	s.Engine.GET("/ws/terminal", HandleWSWithAuth(terminal, s.token))
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
