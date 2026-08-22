package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// One key exhausts its burst alone; another key's budget is untouched.
func TestIPLimiterKeysAreIndependent(t *testing.T) {
	l := newIPLimiter(60, 3)
	for i := range 3 {
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

// A route behind AuthRateLimit answers 429 with the error envelope once an IP
// exceeds the auth budget. The probe stands in for the /auth group, which
// mounts this exact middleware in handler's route registration.
func TestAuthRateLimitAnswers429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/probe", AuthRateLimit(), func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	last, body := 0, ""
	for range authRateBurst + 5 {
		req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		last, body = w.Code, w.Body.String()
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after exceeding burst = %d, want %d", last, http.StatusTooManyRequests)
	}
	if want := `"code":"rate_limited"`; !strings.Contains(body, want) {
		t.Fatalf("429 body %q does not carry %s", body, want)
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
