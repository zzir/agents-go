// Package server wires up the gin HTTP engine, static-file serving, and the
// WebSocket upgrade. REST route registration lives with the handlers
// (handler.Handlers.Register); the server only decides where the API is
// mounted.
package server

import (
	"log/slog"
	"net/http"

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

// NewPreviewEngine builds the engine that serves ONLY sandbox port previews,
// on its own listener and origin (a separate port). It shares nothing with the
// app engine — no static assets, no bearer auth, no app HTML — so the
// untrusted page a preview serves runs on an origin that never held the
// workbench token (decisions §5.37). The bearer middleware is absent because a
// browser tab sends no Authorization header, and the unguessable grant in the
// path authorizes the request. Every response carries Referrer-Policy: no-referrer so a sub-resource
// cannot leak the grant through its Referer, and the per-IP rate limit stands
// in for the missing bearer. trustedProxies mirrors the app engine's, so
// per-IP limiting is correct behind the same proxy.
func NewPreviewEngine(log *slog.Logger, preview gin.HandlerFunc, trustedProxies []string) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	if err := e.SetTrustedProxies(trustedProxies); err != nil {
		return nil, err
	}
	e.Use(gin.Recovery())
	e.Use(logMiddleware(log))
	e.Use(func(c *gin.Context) { c.Header("Referrer-Policy", "no-referrer"); c.Next() })
	e.Any(PreviewPrefix+":token/*path", RateLimit(previewRatePerMinute, previewRateBurst), preview)
	// This origin is not the app: an unmatched path is a plain 404, never the
	// SPA — nothing of the workbench is served here.
	e.NoRoute(func(c *gin.Context) { c.String(http.StatusNotFound, "not found") })
	return e, nil
}

// RegisterWS mounts the WebSocket endpoints with application-level auth (the
// first WS frame, resolved by the same AuthFunc as REST): /ws for run events,
// /ws/terminal for one interactive sandbox terminal per connection.
func (s *Server) RegisterWS(ws, terminal WSHandlerFunc) {
	s.Engine.GET("/ws", HandleWSWithAuth(ws, s.auth, s.guard, s.Conns))
	s.Engine.GET("/ws/terminal", HandleWSWithAuth(terminal, s.auth, s.guard, s.Conns))
}

// ServeOpenAPI mounts the OpenAPI document (auth-exempt) at
// APIPrefix/openapi.yaml.
func (s *Server) ServeOpenAPI(spec []byte) {
	s.Engine.GET(APIPrefix+"/openapi.yaml", func(c *gin.Context) {
		c.Data(200, "application/yaml; charset=utf-8", spec)
	})
}
