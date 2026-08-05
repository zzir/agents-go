package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ResolveApproval's failure modes each keep their own status and reason: the
// five conflicts all answer 409 but must stay distinguishable, a drain is
// retryable, a vanished approval is a 404, and anything else is a 500 whose
// detail stays off the wire. The runner returns these wrapped, so every case
// is matched through a wrap.
func TestResolveErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &ApprovalHandler{}

	cases := []struct {
		name     string
		err      error
		want     int
		contains string
	}{
		{"draining", bridge.ErrShuttingDown{}, http.StatusServiceUnavailable, "shutting down"},
		{"session busy", bridge.ErrSessionBusy{RunID: "r1"}, http.StatusConflict, "r1"},
		{"stale state", &bridge.StaleApprovalStateError{RunID: "r2", WantVersion: "2"}, http.StatusConflict, "predates"},
		{"not resumable", bridge.ErrRunNotResumable{RunID: "r3", Status: bridge.RunCompleted}, http.StatusConflict, "cannot be resumed"},
		{"decision void", &bridge.ApprovalVoidError{TaskID: "t1"}, http.StatusConflict, "void"},
		{"not settled yet", &bridge.ApprovalNotReadyError{RunID: "r4"}, http.StatusConflict, "retry"},
		{"session deleting", bridge.ErrSessionDeleting{SessionID: "s1"}, http.StatusConflict, "being deleted"},
		{"unknown approval", store.ErrNotFound, http.StatusNotFound, "not found"},
		{"transient db error", errors.New("database is locked"), http.StatusInternalServerError, "internal error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			h.resolveError(c, fmt.Errorf("resolving approval: %w", tc.err))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.contains) {
				t.Errorf("body = %s, want it to mention %q", w.Body.String(), tc.contains)
			}
		})
	}
}
