package agents

import (
	"context"
	"strings"
	"testing"
)

// A nil entry in Agent.Tools is a construction bug (a conditional append gone
// wrong); the run reports it by name instead of panicking on a field read.
func TestRun_NilToolEntryIsNamed(t *testing.T) {
	agent := &Agent{Name: "a", Tools: []*FunctionTool{nil}, ModelImpl: &fakeModel{}}
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "Tools[0] is nil") {
		t.Fatalf("err = %v, want a named nil-tool error", err)
	}
}
