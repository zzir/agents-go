package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// One key exhausts its burst alone; another key's budget is untouched.
func TestIPLimiterKeysAreIndependent(t *testing.T) {
	l := newIPLimiter(60, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d for key a should pass within burst", i+1)
		}
	}
	if l.Allow("a") {
		t.Fatal("key a should be over its burst")
	}
	if !l.Allow("b") {
		t.Fatal("key b must not share key a's budget")
	}
}

// The auth surface answers 429 with the error envelope once an IP exceeds its
// budget — it is the one place credentials can be guessed.
func TestAuthEndpointsRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), "tok")

	last := 0
	for i := 0; i < authRateBurst+5; i++ {
		req := httptest.NewRequest(http.MethodPost, APIPrefix+"/auth/login", strings.NewReader(`{"token":"nope"}`))
		w := httptest.NewRecorder()
		s.Engine.ServeHTTP(w, req)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after exceeding burst = %d, want %d", last, http.StatusTooManyRequests)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	good := map[string]string{
		"http://localhost:9527":       "http://localhost:9527",
		"https://agents.example.com":  "https://agents.example.com",
		"https://agents.example.com/": "https://agents.example.com",
	}
	for in, want := range good {
		got, err := NormalizeBaseURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"agents.example.com",             // no scheme
		"ftp://agents.example.com",       // wrong scheme
		"https://agents.example.com/app", // path: app assumes root mount
		"https://user@agents.example.com",
		"https://agents.example.com?x=1",
	}
	for _, in := range bad {
		if _, err := NormalizeBaseURL(in); err == nil {
			t.Errorf("NormalizeBaseURL(%q) should fail", in)
		}
	}
}
