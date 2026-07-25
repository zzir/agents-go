package bridge

import (
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// A guardrail tripwire is classified into a distinct code + name + stage so the
// UI can render a "blocked" state; any other error keeps the fallback code.
func TestGuardrailRunError(t *testing.T) {
	inTrip := &agents.GuardrailTripwireError{
		Result: agents.GuardrailResult{
			Stage:     agents.StageInput,
			Guardrail: agents.Guardrail{Name: "no_pii"},
		},
	}
	if e := runErrorFor("r1", inTrip, "stream_error"); e.Code != "guardrail_tripwire" || e.Guardrail != "no_pii" || e.Stage != "input" {
		t.Errorf("input trip = %+v, want code=guardrail_tripwire guardrail=no_pii stage=input", e)
	}

	outTrip := &agents.GuardrailTripwireError{
		Result: agents.GuardrailResult{
			Stage:     agents.StageOutput,
			Guardrail: agents.Guardrail{Name: "no_secrets"},
		},
	}
	if e := runErrorFor("r1", outTrip, "stream_error"); e.Code != "guardrail_tripwire" || e.Guardrail != "no_secrets" || e.Stage != "output" {
		t.Errorf("output trip = %+v, want code=guardrail_tripwire guardrail=no_secrets stage=output", e)
	}

	if e := runErrorFor("r1", errors.New("boom"), "resume_error"); e.Code != "resume_error" || e.Guardrail != "" || e.Stage != "" {
		t.Errorf("plain error = %+v, want fallback code resume_error, no guardrail fields", e)
	}
}
