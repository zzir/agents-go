// Package server wires up the gin HTTP engine, static-file serving, and the
// WebSocket upgrade. REST route registration lives with the handlers
// (handler.Handlers.Register); the server only decides where the API is
// mounted.
package server

import (
	"github.com/gin-gonic/gin"
)

// APIPrefix is where the REST API is mounted; the one place the path is
// spelled — the auth exemptions and the auth routes read it too.
const APIPrefix = "/api/v1"

// RegisterAPI mounts register under APIPrefix.
func (s *Server) RegisterAPI(register func(*gin.RouterGroup)) {
	register(s.Engine.Group(APIPrefix))
}

// HooksPrefix is where webhook triggers are called — outside APIPrefix, so
// the token middleware does not guard it: a hook proves itself by signature.
const HooksPrefix = "/hooks"

// RegisterHook mounts the webhook endpoint, POST HooksPrefix/:id. It is
// auth-exempt (HMAC self-authenticating), so it carries its own per-IP rate
// limit — signature verification alone would let anyone spend our reads.
func (s *Server) RegisterHook(hook gin.HandlerFunc) {
	s.Engine.POST(HooksPrefix+"/:id", RateLimit(hookRatePerMinute, hookRateBurst), hook)
}

// RegisterWS mounts the WebSocket endpoints with token auth: /ws for run
// events, /ws/terminal for one interactive sandbox terminal per connection.
func (s *Server) RegisterWS(ws, terminal WSHandlerFunc) {
	s.Engine.GET("/ws", HandleWSWithAuth(ws, s.token))
	s.Engine.GET("/ws/terminal", HandleWSWithAuth(terminal, s.token))
}

// ServeOpenAPI mounts the OpenAPI document (auth-exempt) at
// APIPrefix/openapi.yaml.
func (s *Server) ServeOpenAPI(spec []byte) {
	s.Engine.GET(APIPrefix+"/openapi.yaml", func(c *gin.Context) {
		c.Data(200, "application/yaml; charset=utf-8", spec)
	})
}
