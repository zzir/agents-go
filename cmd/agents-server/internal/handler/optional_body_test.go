package handler

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// An optional body is optional, not ignorable: one that is present and does
// not parse is refused before it can read as the default. The handlers have no
// dependencies wired, so a request that got past the bind would panic.
func TestOptionalBodyRefusesBadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := newTestEngine()
	engine.POST("/tasks/:id/stop", wrap((&TaskHandler{}).Stop))
	engine.POST("/approvals/:tool_call_id/approve", wrap((&ApprovalHandler{}).Approve))
	engine.POST("/approvals/:tool_call_id/reject", wrap((&ApprovalHandler{}).Reject))

	for _, path := range []string{"/tasks/t/stop", "/approvals/c/approve", "/approvals/c/reject"} {
		w := doJSON(t, engine, http.MethodPost, path, `{"graceful": tru`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST %s with a truncated body: got %d, want 400 (%s)", path, w.Code, w.Body.String())
		}
	}
}

// bindOptional tells an absent body (the zero request) from a present one.
func TestBindOptional(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := newTestEngine()
	engine.POST("/x", wrap(func(c *gin.Context) error {
		var req taskStopReq
		if err := bindOptional(c, &req); err != nil {
			return err
		}
		c.JSON(http.StatusOK, req)
		return nil
	}))
	for body, want := range map[string]string{
		"":                  `{"graceful":false}`,
		`{"graceful":true}`: `{"graceful":true}`,
	} {
		w := doJSON(t, engine, http.MethodPost, "/x", body)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("body %q: got %d %s, want 200 %s", body, w.Code, w.Body.String(), want)
		}
	}
}
