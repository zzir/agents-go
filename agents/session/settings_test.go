package session_test

import (
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// ResolveLimit's whole remaining job is the clamp. Settings.Limit is a plain
// count, but the Cursor it feeds reads a negative limit as "the most recent
// -Limit"; a negative that survived would come back through the negation at
// the call site as a POSITIVE cursor limit, loading the oldest entries.
func TestResolveLimitClampsToNoLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   session.Settings
		want int
	}{
		{"the zero value is no limit", session.Settings{}, 0},
		{"a positive limit passes through", session.Settings{Limit: 7}, 7},
		{"a negative limit is no limit, not a reversal", session.Settings{Limit: -7}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := session.ResolveLimit(tc.in); got != tc.want {
				t.Errorf("ResolveLimit(%+v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
