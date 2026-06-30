package server

import (
	"crypto/subtle"
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
	auth := s.Engine.Group("/api/auth")
	auth.POST("/login", func(c *gin.Context) {
		var req struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	auth.GET("/check", func(c *gin.Context) {
		if subtle.ConstantTimeCompare([]byte(extractToken(c)), []byte(s.token)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}

// ServeStatic serves files from staticFS, falling back to index.html for unmatched routes (SPA support).
// Text assets (.js, .css, .html, .svg, .json) may be pre-compressed as .br files;
// they are served transparently with Content-Encoding: br.
func (s *Server) ServeStatic(staticFS fs.FS) {
	httpFS := http.FS(staticFS)
	s.Engine.NoRoute(func(c *gin.Context) {
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
	if data, err := fs.ReadFile(sfs, p+".br"); err == nil {
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		c.Header("Content-Encoding", "br")
		c.Header("Vary", "Accept-Encoding")
		c.Data(http.StatusOK, ct, data)
		return true
	}
	return false
}

func cspMiddleware() gin.HandlerFunc {
	const policy = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; " +
		"connect-src 'self' ws: wss:; " +
		"font-src 'self' data:"
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", policy)
		c.Next()
	}
}

func redactQuery(u *url.URL) string {
	q := u.Query()
	if q.Get("token") != "" {
		q.Set("token", "REDACTED")
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

func zerologMiddleware(log zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		log.Info().
			Str("method", c.Request.Method).
			Str("path", redactQuery(c.Request.URL)).
			Int("status", c.Writer.Status()).
			Msg("request")
	}
}
