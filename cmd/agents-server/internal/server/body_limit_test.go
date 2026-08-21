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
// memory whole — every JSON endpoint binds the body without its own limit.
func TestBodyLimitRejectsOversizedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), "tok")

	huge := `{"token":"` + strings.Repeat("a", maxBodyBytes+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/auth/login", strings.NewReader(huge))
	w := httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	// A normal-sized body still reaches the handler (wrong token → 401, not 400).
	req = httptest.NewRequest(http.MethodPost, APIPrefix+"/auth/login", strings.NewReader(`{"token":"nope"}`))
	w = httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}
