package server

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

// Server wraps the gin engine and logger.
type Server struct {
	Engine *gin.Engine
	Log    zerolog.Logger
	token  string
}

// New creates a Server with a gin engine configured for release mode, recovery, and request logging.
func New(log zerolog.Logger, token string) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(cspMiddleware())
	engine.Use(zerologMiddleware(log))
	engine.Use(TokenAuth(token))
	s := &Server{Engine: engine, Log: log, token: token}
	s.registerAuthRoutes()
	return s
}

func (s *Server) registerAuthRoutes() {
	login := func(c *gin.Context) {
		var req struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "validation", "message": "invalid request"}})
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "unauthorized", "message": "invalid token"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	check := func(c *gin.Context) {
		if subtle.ConstantTimeCompare([]byte(extractToken(c)), []byte(s.token)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "unauthorized", "message": "unauthorized"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	for _, prefix := range []string{"/api/v1/auth", "/api/auth"} {
		auth := s.Engine.Group(prefix)
		auth.POST("/login", login)
		auth.GET("/check", check)
	}
}

// ServeHealth mounts an unauthenticated liveness endpoint at /health that
// reports the server status and build version — for container probes and load
// balancers. It carries no sensitive data and is safe to expose.
func (s *Server) ServeHealth(version string) {
	s.Engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "version": version})
	})
}

// ServeStatic serves files from staticFS, falling back to index.html for unmatched routes (SPA support).
// Text assets (.js, .css, .html, .svg, .json) may be pre-compressed as .br files;
// they are served transparently with Content-Encoding: br.
func (s *Server) ServeStatic(staticFS fs.FS) {
	httpFS := http.FS(staticFS)
	s.Engine.NoRoute(func(c *gin.Context) {
		// Unmatched API paths are client errors, not SPA routes: answer with a
		// JSON 404 so a removed/mistyped endpoint doesn't return index.html.
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": "not found"}})
			return
		}
		p := c.Request.URL.Path[1:]
		if p == "" {
			p = "index.html"
		}
		if serveAsset(c, staticFS, httpFS, p) {
			return
		}
		serveAsset(c, staticFS, httpFS, "index.html")
	})
}

func serveAsset(c *gin.Context, sfs fs.FS, httpFS http.FileSystem, p string) bool {
	if strings.HasPrefix(p, "assets/") {
		c.Header("Cache-Control", "max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "no-store")
	}
	if f, err := sfs.Open(p); err == nil {
		_ = f.Close()
		c.FileFromFS("/"+p, httpFS)
		return true
	}
	if data, err := fs.ReadFile(sfs, p+".gz"); err == nil {
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Header("Content-Encoding", "gzip")
			c.Header("Vary", "Accept-Encoding")
			c.Data(http.StatusOK, ct, data)
		} else {
			gr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				c.Status(http.StatusInternalServerError)
				return true
			}
			raw, err := io.ReadAll(gr)
			_ = gr.Close()
			if err != nil {
				c.Status(http.StatusInternalServerError)
				return true
			}
			c.Data(http.StatusOK, ct, raw)
		}
		return true
	}
	return false
}

func cspMiddleware() gin.HandlerFunc {
	// connect-src is same-origin: the SPA only talks to this server (REST over
	// http(s), live updates over the ws/wss upgrade of the same origin), so
	// 'self' already covers same-origin WebSockets in modern browsers.
	const policy = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; " +
		"connect-src 'self'; " +
		"font-src 'self' data:"
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", policy)
		c.Next()
	}
}

// redactSensitiveQueryKeys are query parameters scrubbed from request logs:
// the auth token, and the OAuth authorization code/state that ride the MCP and
// ChatGPT OAuth callback redirects (a leaked code is a usable credential).
var redactSensitiveQueryKeys = []string{"token", "code", "state"}

func redactQuery(u *url.URL) string {
	q := u.Query()
	for _, key := range redactSensitiveQueryKeys {
		if q.Get(key) != "" {
			q.Set(key, "REDACTED")
		}
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

func zerologMiddleware(log zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Make the logger reachable via zerolog.Ctx from handlers and from
		// everything derived from the request context (e.g. WS connections).
		c.Request = c.Request.WithContext(log.WithContext(c.Request.Context()))
		c.Next()
		log.Info().
			Str("method", c.Request.Method).
			Str("path", redactQuery(c.Request.URL)).
			Int("status", c.Writer.Status()).
			Msg("request")
	}
}
