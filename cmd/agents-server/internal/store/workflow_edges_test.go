package store

import "testing"

func edgeWorkflow(t *testing.T, steps ...WorkflowStep) *Workflow {
	t.Helper()
	w := &Workflow{Name: "w", Description: "d", Steps: steps}
	return w
}

// The empty defaults ARE the linear workflow: success falls through, failure
// stops. Everything else is an explicit edge.
func TestNextStepFollowsTheEdges(t *testing.T) {
	steps := WorkflowSteps{
		{ID: "write", AgentConfigID: "a", Prompt: "p"},
		{ID: "test", AgentConfigID: "a", Prompt: "p", OnSuccess: WorkflowStepEnd, OnFailure: "fix"},
		{ID: "fix", AgentConfigID: "a", Prompt: "p", OnSuccess: "test"},
	}
	run := &WorkflowRun{Steps: steps}

	cases := []struct {
		at     string
		failed bool
		want   string // "" = the execution ends
	}{
		{at: "write", want: "test"},             // empty on_success falls through
		{at: "write", failed: true, want: ""},   // empty on_failure ends it
		{at: "test", want: ""},                  // on_success: end
		{at: "test", failed: true, want: "fix"}, // routed to the handler
		{at: "fix", want: "test"},               // and back — the loop
		{at: "fix", failed: true, want: ""},     // the handler itself failing ends it
	}
	for _, c := range cases {
		run.StepID = c.at
		next, ok := run.NextStep(c.failed)
		if c.want == "" {
			if ok {
				t.Errorf("at %q (failed=%v): went to %q, want the execution to end", c.at, c.failed, next.ID)
			}
			continue
		}
		if !ok || next.ID != c.want {
			t.Errorf("at %q (failed=%v): next = %v, want %q", c.at, c.failed, next, c.want)
		}
	}
}

// The last step with no edge ends the execution — the pre-edge behavior.
func TestNextStepEndsAtTheLastStep(t *testing.T) {
	run := &WorkflowRun{Steps: WorkflowSteps{{ID: "only", AgentConfigID: "a", Prompt: "p"}}, StepID: "only"}
	if _, ok := run.NextStep(false); ok {
		t.Fatal("the last step must end the execution")
	}
}

func TestNormalizeWorkflowChecksEdges(t *testing.T) {
	// A backward edge is the point — that is how a sequence loops.
	back := edgeWorkflow(t,
		WorkflowStep{ID: "a", AgentConfigID: "x", Prompt: "p", OnFailure: "b"},
		WorkflowStep{ID: "b", AgentConfigID: "x", Prompt: "p", OnSuccess: "a"},
	)
	if err := NormalizeWorkflow(back); err != nil {
		t.Fatalf("a loop must be accepted: %v", err)
	}

	dangling := edgeWorkflow(t, WorkflowStep{ID: "a", AgentConfigID: "x", Prompt: "p", OnFailure: "nope"})
	if err := NormalizeWorkflow(dangling); err == nil {
		t.Fatal("an edge naming no step must be refused at save time")
	}

	reserved := edgeWorkflow(t, WorkflowStep{ID: WorkflowStepEnd, AgentConfigID: "x", Prompt: "p"})
	if err := NormalizeWorkflow(reserved); err == nil {
		t.Fatalf("%q is reserved as an edge target and must not be a step id", WorkflowStepEnd)
	}

	// end is always a valid target.
	fine := edgeWorkflow(t, WorkflowStep{ID: "a", AgentConfigID: "x", Prompt: "p", OnSuccess: WorkflowStepEnd})
	if err := NormalizeWorkflow(fine); err != nil {
		t.Fatalf("end must be a valid target: %v", err)
	}
}
