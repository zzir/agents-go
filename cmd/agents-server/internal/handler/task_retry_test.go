package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A retry refused by the task's own state is a conflict whose reason the caller
// can act on; an unknown id is a 404; anything else is a fault whose detail
// stays off the wire. The runner returns these wrapped, so each is matched
// through a wrap.
func TestRetryErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &TaskHandler{}

	cases := []struct {
		name     string
		err      error
		want     int
		contains string
	}{
		{"not failed", tasks.ErrNotRetryable{Status: tasks.StatusCompleted}, http.StatusConflict, "only a failed task"},
		{"out of attempts", tasks.ErrRetryLimit{Limit: 3}, http.StatusConflict, "3 attempts"},
		{"parent at the cap", tasks.ErrTaskLimit{Limit: 6}, http.StatusConflict, "6 tasks running"},
		// A workflow execution its budget or step ceiling stopped is refused
		// before a run — a refusal, with its reason, not a fault.
		{"budget spent", fmt.Errorf("%w: 3 of 3 steps", store.ErrBudgetExhausted), http.StatusConflict, "budget exhausted: 3 of 3 steps"},
		{"looping", fmt.Errorf("stopped after 50 steps — %w", store.ErrStepCeiling), http.StatusConflict, "looping"},
		{"unknown task", tasks.ErrNotFound, http.StatusNotFound, "not found"},
		// The adapter maps the store's sentinel to the SDK's, but a caller
		// reaching the row directly can still surface the store's.
		{"unknown row", store.ErrNotFound, http.StatusNotFound, "not found"},
		{"transient db error", errors.New("database is locked"), http.StatusInternalServerError, "internal error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			h.retryError(c, fmt.Errorf("retrying task: %w", tc.err))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if !strings.Contains(strings.ToLower(w.Body.String()), strings.ToLower(tc.contains)) {
				t.Errorf("body = %s, want it to mention %q", w.Body.String(), tc.contains)
			}
		})
	}
}

// An unknown task id must answer 404 on stop too. It used to be matched
// against the store's sentinel, which the adapter never returns — so the one
// case the branch existed for fell through to a 500.
func TestStopErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &TaskHandler{}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"already final", &bridge.TaskFinalError{Status: "completed"}, http.StatusConflict},
		{"unknown task", tasks.ErrNotFound, http.StatusNotFound},
		{"unknown row", store.ErrNotFound, http.StatusNotFound},
		{"transient db error", errors.New("database is locked"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			h.stopError(c, fmt.Errorf("stopping task: %w", tc.err))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
