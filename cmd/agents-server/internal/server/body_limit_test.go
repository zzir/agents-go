package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// A body past the global cap must fail the request instead of being read into
// memory whole — the JSON endpoints bind bodies without their own limits. The
// probe route stands in for any of them; the middleware is engine-wide.
func TestBodyLimitRejectsOversizedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), staticAuth("tok"), nil)
	s.Engine.POST("/probe", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.String(http.StatusBadRequest, "too big or malformed")
			return
		}
		c.String(http.StatusOK, "ok")
	})

	// A declared length past the cap is refused before a byte is read; an
	// undeclared (chunked) body is cut at the cap and fails the decode.
	huge := `{"x":"` + strings.Repeat("a", maxBodyBytes+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(huge))
	w := httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	req = httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(huge))
	req.ContentLength = -1
	w = httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("undeclared oversize status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	// A path with its own, larger limit takes what the default refuses.
	s.SetBodyLimit("/wide", func() int64 { return maxBodyBytes * 4 })
	s.Engine.POST("/wide", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.String(http.StatusBadRequest, "too big or malformed")
			return
		}
		c.String(http.StatusOK, "ok")
	})
	req = httptest.NewRequest(http.MethodPost, "/wide", strings.NewReader(huge))
	w = httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("wide route status = %d, want 200", w.Code)
	}

	// A normal-sized body still reaches the handler intact.
	req = httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(`{"x":"y"}`))
	w = httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
