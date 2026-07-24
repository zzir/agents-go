package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// StartRun's failure modes must map to distinct status codes rather than
// all collapsing to 404 "session not found". A busy session or a task-limit hit
// is a 409, an unknown session is a 404, and any other DB error is a 500.
func TestStartErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &RunHandler{}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"session busy", bridge.ErrSessionBusy{RunID: "r1"}, http.StatusConflict},
		{"task limit", bridge.ErrTaskLimit{Limit: 6}, http.StatusConflict},
		{"unknown session", fmt.Errorf("getting session s1: %w", store.ErrNotFound), http.StatusNotFound},
		{"transient db error", errors.New("database is locked"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		h.startError(c, tc.err)
		if w.Code != tc.want {
			t.Errorf("%s: startError -> %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}
