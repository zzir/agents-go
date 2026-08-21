package server

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// Server wraps the gin engine.
type Server struct {
	Engine *gin.Engine
	token  string
	// cspPolicy is the Content-Security-Policy every response carries; base
	// policy from New, extended by ServeStatic with the hashes of the served
	// page's inline scripts.
	cspPolicy string
}

// New creates a Server with a gin engine configured for release mode, recovery, and request logging.
func New(log *slog.Logger, token string) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	s := &Server{Engine: engine, token: token, cspPolicy: buildCSP(nil)}
	engine.Use(s.cspMiddleware())
	engine.Use(logMiddleware(log))
	engine.Use(TokenAuth(token))
	s.registerAuthRoutes()
	return s
}

func (s *Server) registerAuthRoutes() {
	login := func(c *gin.Context) {
		var req struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, protocol.NewErrorResponse(protocol.CodeValidation, "invalid request"))
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
			c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "invalid token"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	check := func(c *gin.Context) {
		if subtle.ConstantTimeCompare([]byte(extractToken(c)), []byte(s.token)) != 1 {
			c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "unauthorized"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	auth := s.Engine.Group(APIPrefix + "/auth")
	auth.POST("/login", login)
	auth.GET("/check", check)
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
	s.cspPolicy = buildCSP(inlineScriptHashes(staticFS))
	httpFS := http.FS(staticFS)
	s.Engine.NoRoute(func(c *gin.Context) {
		// Unmatched API paths are client errors, not SPA routes: answer with a
		// JSON 404 so a removed/mistyped endpoint doesn't return index.html.
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, protocol.NewErrorResponse(protocol.CodeNotFound, "not found"))
			return
		}
		p := c.Request.URL.Path[1:]
		p = cmp.Or(p, "index.html")
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
		ct = cmp.Or(ct, "application/octet-stream")
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

// buildCSP renders the policy; scriptHashes extends script-src with the
// sha256 sources of the page's inline scripts.
func buildCSP(scriptHashes []string) string {
	// connect-src is same-origin: the SPA only talks to this server (REST over
	// http(s), live updates over the ws/wss upgrade of the same origin), so
	// 'self' already covers same-origin WebSockets in modern browsers.
	scriptSrc := "script-src 'self'"
	for _, h := range scriptHashes {
		scriptSrc += " 'sha256-" + h + "'"
	}
	return "default-src 'self'; " +
		scriptSrc + "; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; " +
		"connect-src 'self'; " +
		"font-src 'self' data:"
}

func (s *Server) cspMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", s.cspPolicy)
		c.Next()
	}
}

// inlineScriptRE matches bare inline <script> blocks; tagged scripts
// (type="module", src=…) are external and covered by 'self'.
var inlineScriptRE = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// inlineScriptHashes hashes every inline <script> of the FS's index.html —
// computed from the same bytes the server serves, so the policy cannot drift
// from the page. (index.html's theme-init must stay inline and synchronous:
// it gates first paint.)
func inlineScriptHashes(fsys fs.FS) []string {
	html := readIndexHTML(fsys)
	if html == nil {
		return nil
	}
	var hashes []string
	for _, m := range inlineScriptRE.FindAllSubmatch(html, -1) {
		sum := sha256.Sum256(m[1])
		hashes = append(hashes, base64.StdEncoding.EncodeToString(sum[:]))
	}
	return hashes
}

func readIndexHTML(fsys fs.FS) []byte {
	if raw, err := fs.ReadFile(fsys, "index.html"); err == nil {
		return raw
	}
	raw, err := fs.ReadFile(fsys, "index.html.gz")
	if err != nil {
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	html, err := io.ReadAll(zr)
	if err != nil {
		return nil
	}
	return html
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

func logMiddleware(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Make the logger reachable via logging.Ctx from handlers and from
		// everything derived from the request context (e.g. WS connections).
		c.Request = c.Request.WithContext(logging.Into(c.Request.Context(), log))
		c.Next()
		log.Info("request", "method", c.Request.Method, "path", redactQuery(c.Request.URL), "status", c.Writer.Status())
	}
}
