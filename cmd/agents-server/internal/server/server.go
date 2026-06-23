package server

import (
	"io/fs"
	"mime"
	"net/http"
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
}

// New creates a Server with a gin engine configured for release mode, recovery, and request logging.
func New(log zerolog.Logger) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(zerologMiddleware(log))
	return &Server{Engine: engine, Log: log}
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
			c.FileFromFS("index.html", httpFS)
			return
		}
		_ = f.Close()
		ext := filepath.Ext(path)
		if ct := mime.TypeByExtension(ext); ct != "" {
			c.Header("Content-Type", ct)
		}
		c.FileFromFS(c.Request.URL.Path, httpFS)
	})
}

func zerologMiddleware(log zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		log.Info().
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Int("status", c.Writer.Status()).
			Msg("request")
	}
}
