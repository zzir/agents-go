package server

import (
	"crypto/subtle"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

func init() {
	_ = mime.AddExtensionType(".jsx", "application/javascript")
}

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
func (s *Server) ServeStatic(staticFS fs.FS) {
	httpFS := http.FS(staticFS)
	s.Engine.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path[1:]
		if path == "" {
			path = "index.html"
		}
		f, err := staticFS.Open(path)
		if err != nil {
			c.Header("Cache-Control", "no-store")
			c.FileFromFS("index.html", httpFS)
			return
		}
		_ = f.Close()
		ext := filepath.Ext(path)
		if ct := mime.TypeByExtension(ext); ct != "" {
			c.Header("Content-Type", ct)
		}
		c.Header("Cache-Control", "no-store")
		c.FileFromFS(c.Request.URL.Path, httpFS)
	})
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
