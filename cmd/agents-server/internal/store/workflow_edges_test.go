package store

import (
	"strings"
	"testing"
)

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
	run := &WorkflowState{Steps: steps}

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
	run := &WorkflowState{Steps: WorkflowSteps{{ID: "only", AgentConfigID: "a", Prompt: "p"}}, StepID: "only"}
	if _, ok := run.NextStep(false); ok {
		t.Fatal("the last step must end the execution")
	}
}

// A gate's words are trimmed, one line each, and distinct — the checks that
// keep both edges reachable.
func TestNormalizeWorkflowChecksGateWords(t *testing.T) {
	mk := func(pass, fail string) *Workflow {
		return &Workflow{Name: "w", Description: "d", Steps: WorkflowSteps{{ID: "a", AgentConfigID: "x", Prompt: "p", Gate: &StepGate{Pass: pass, Fail: fail}}}}
	}
	ok := mk(" ok ", " no ")
	if err := NormalizeWorkflow(ok); err != nil || ok.Steps[0].Gate.Pass != "ok" || ok.Steps[0].Gate.Fail != "no" {
		t.Fatalf("trimmed words: %v %+v", err, ok.Steps[0].Gate)
	}
	// Punctuation Verdict strips off a line is stripped off the word too, so
	// "OK!" is reported as "OK" and matched as "OK" — from either side.
	punct := mk("OK!", "**NOPE**")
	if err := NormalizeWorkflow(punct); err != nil || punct.Steps[0].Gate.Pass != "OK" || punct.Steps[0].Gate.Fail != "NOPE" {
		t.Fatalf("punctuated words: %v %+v", err, punct.Steps[0].Gate)
	}
	if passed, ok := (&StepGate{Pass: "OK!", Fail: "NOPE."}).Verdict("all good\nOK!"); !ok || !passed {
		t.Fatalf("a stored punctuated word must still match: %v %v", passed, ok)
	}
	if err := NormalizeWorkflow(mk("!!!", "no")); err == nil {
		t.Fatal("a word that is only punctuation must be refused")
	}
	for name, wf := range map[string]*Workflow{
		"same words":               mk("done", "DONE"),
		"pass equals default fail": mk("FAIL", ""),
		"multi-line":               mk("yes\nplease", "no"),
	} {
		if err := NormalizeWorkflow(wf); err == nil {
			t.Errorf("%s: accepted", name)
		}
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

// A gate reads the LAST non-empty line, forgivingly on decoration and case, and
// refuses to guess when neither sentinel is there.
func TestGateVerdictReadsTheLastLine(t *testing.T) {
	g := &StepGate{}
	cases := []struct {
		out    string
		passed bool
		ok     bool
	}{
		{"all good\nPASS", true, true},
		{"all good\n\n**PASS**\n\n", true, true},
		{"nope\nfail.", false, true},
		{"Fail", false, true},
		{"the tests pass", false, false}, // a sentence, not a verdict
		{"PASS\nbut then more text", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		passed, ok := g.Verdict(c.out)
		if passed != c.passed || ok != c.ok {
			t.Errorf("Verdict(%q) = %v,%v want %v,%v", c.out, passed, ok, c.passed, c.ok)
		}
	}
	// A structured answer carries the verdict as a field — bare or fenced.
	for _, c := range []struct {
		out    string
		passed bool
	}{
		{`{"passed": true, "notes": "all green"}`, true},
		{"```json\n{\"passed\": false}\n```", false},
		{`{"verdict": "FAIL", "reason": "two tests red"}`, false},
		{`{"result": "pass"}`, true},
	} {
		if passed, ok := g.Verdict(c.out); !ok || passed != c.passed {
			t.Errorf("Verdict(%q) = %v,%v want %v,true", c.out, passed, ok, c.passed)
		}
	}
	// A JSON object with no verdict field is no verdict, not a guess.
	if _, ok := g.Verdict(`{"notes": "looks fine"}`); ok {
		t.Fatal("an object without a verdict field must not be read as one")
	}
	custom := &StepGate{Pass: "SHIP", Fail: "HOLD"}
	if passed, ok := custom.Verdict("looks fine\nship"); !passed || !ok {
		t.Fatal("custom sentinels must be read")
	}
	if _, ok := custom.Verdict("PASS"); ok {
		t.Fatal("the default sentinel is not a verdict for a gate that renamed it")
	}
}

// A gated step's prompt carries the instruction to report, so the author does
// not have to remember it; a plain step's does not.
func TestStepPromptCarriesTheGateInstruction(t *testing.T) {
	st := &WorkflowState{Input: "the brief", Steps: WorkflowSteps{
		{ID: "a", AgentConfigID: "x", Prompt: "do"},
		{ID: "b", AgentConfigID: "x", Prompt: "check", Gate: &StepGate{}},
	}}
	if got := st.StepPrompt(st.Steps[0]); got != "the brief\n\ndo" {
		t.Fatalf("plain first step prompt = %q", got)
	}
	got := st.StepPrompt(st.Steps[1])
	if !strings.HasPrefix(got, "check\n\n") || !strings.Contains(got, "PASS") || !strings.Contains(got, "FAIL") {
		t.Fatalf("gated step prompt = %q, want the verdict instruction appended", got)
	}
}
