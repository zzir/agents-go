package handler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The table every handler's failure goes through, pinned: a sentinel keeps its
// status however deeply it is wrapped, and an unknown error leaks nothing.
func TestClassify(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("updating row: %w", err) }
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		message string // "" = not checked
	}{
		{"missing row", wrapped(store.ErrNotFound), http.StatusNotFound, "not found"},
		{"stale revision", wrapped(store.ErrRevisionConflict), http.StatusConflict, ""},
		{"owner moved", wrapped(store.ErrOwnershipChanged), http.StatusConflict, ""},
		{"same scope", wrapped(store.ErrSameScope), http.StatusConflict, ""},
		{"group exists", wrapped(store.ErrGroupExists), http.StatusConflict, ""},
		{"sandbox move", wrapped(store.ErrSandboxMoveDestination), http.StatusConflict, ""},
		{"provider gone", wrapped(store.ErrProviderRef), http.StatusBadRequest, ""},
		{"provider out of scope", wrapped(store.ErrProviderScope), http.StatusBadRequest, ""},
		{"owner is nobody", wrapped(store.ErrNoSuchUser), http.StatusBadRequest, "no such user"},
		// The bridge's refusals to start or resume work answer the same from every route.
		{"session busy", wrapped(bridge.ErrSessionBusy{RunID: "r1"}), http.StatusConflict, "session already has an active run: r1"},
		{"session fenced", wrapped(bridge.ErrSessionBusy{}), http.StatusConflict, ""},
		{"session deleting", wrapped(bridge.ErrSessionDeleting{SessionID: "s1"}), http.StatusConflict, ""},
		{"draining", wrapped(bridge.ErrShuttingDown{}), http.StatusServiceUnavailable, "server is shutting down"},
		{"binding that cannot work", wrapped(bridge.ErrInvalidBinding{Reason: "workspace not found: w1"}), http.StatusBadRequest, ""},
		{"binding lost its race", wrapped(bridge.ErrBindingContention), http.StatusConflict, ""},
		{"task cap", wrapped(tasks.ErrTaskLimit), http.StatusConflict, ""},
		{"unknown session", fmt.Errorf("getting session s1: %w", store.ErrNotFound), http.StatusNotFound, ""},
		{"a handler's own answer", wrapped(badRequest("name is required")), http.StatusBadRequest, "name is required"},
		{"a handler's answer outranks the table", fmt.Errorf("%w: %w", conflict("busy"), store.ErrNotFound), http.StatusConflict, "busy"},
		{"anything else", errors.New("database is locked"), http.StatusInternalServerError, "internal error"},
	} {
		got := classify(tc.err)
		if got.status != tc.status || (tc.message != "" && got.message != tc.message) {
			t.Errorf("%s: classify = %d %q, want %d %q", tc.name, got.status, got.message, tc.status, tc.message)
		}
	}
}
