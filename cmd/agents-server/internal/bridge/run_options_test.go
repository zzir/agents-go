package bridge

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

// Unset is the SDK default (notice, then a closing turn); abort keeps the old
// fatal stop; off disables both valves.
func TestToolLoopPolicyMapsTheAgentSetting(t *testing.T) {
	cases := map[string]agents.ToolLoopPolicy{
		"":      {},
		"abort": {Abort: true},
		"off":   {MaxRepeatedCalls: -1, MaxConsecutiveFailedTurns: -1},
	}
	for setting, want := range cases {
		if got := toolLoopPolicy(setting); got != want {
			t.Errorf("toolLoopPolicy(%q) = %+v, want %+v", setting, got, want)
		}
	}
}
