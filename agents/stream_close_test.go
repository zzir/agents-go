package agents

import "testing"

// After the terminal event went out the iterator has returned; a tool
// goroutine that outlived its call must find the stream closed rather than
// call a yield that is gone.
func TestEmit_AfterTheStreamFinishedIsIgnored(t *testing.T) {
	yields := 0
	r := &runner{ctrl: newRunControl(), yield: func(StreamEvent, error) bool {
		yields++
		return true
	}}
	r.finishStream(&RunResult{}, nil)
	if yields != 1 {
		t.Fatalf("terminal yields = %d, want 1", yields)
	}
	if r.emit(&ItemsPersistedEvent{}) {
		t.Error("emit after the terminal event reported a live consumer")
	}
	if yields != 1 {
		t.Errorf("yield was called %d times; the late emit reached the returned iterator", yields)
	}
}
