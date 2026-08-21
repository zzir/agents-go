package openai

import (
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestConvertPrompt(t *testing.T) {
	p, err := convertPrompt(&agents.Prompt{
		ID:        "pmpt_123",
		Version:   "2",
		Variables: map[string]any{"city": "Paris"},
	})
	if err != nil {
		t.Fatalf("convertPrompt: %v", err)
	}
	if p.ID != "pmpt_123" {
		t.Errorf("ID = %q", p.ID)
	}
	if p.Version.Value != "2" {
		t.Errorf("Version = %q", p.Version.Value)
	}
	if got := p.Variables["city"].OfString.Value; got != "Paris" {
		t.Errorf("city = %q", got)
	}
}

func TestConvertPromptRejectsNonStringVariable(t *testing.T) {
	_, err := convertPrompt(&agents.Prompt{
		ID:        "pmpt_123",
		Variables: map[string]any{"n": 7},
	})
	if err == nil {
		t.Fatal("expected error for non-string variable, got nil")
	}
	if _, ok := errors.AsType[*agents.UserError](err); !ok {
		t.Fatalf("error = %T, want *agents.UserError", err)
	}
}

func TestBuildParamsIncludesPrompt(t *testing.T) {
	m := &ResponsesModel{model: "gpt-4o"}
	params, err := m.buildParams(agents.ModelRequest{
		Prompt: &agents.Prompt{ID: "pmpt_abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if params.Prompt.ID != "pmpt_abc" {
		t.Errorf("params.Prompt.ID = %q", params.Prompt.ID)
	}
}
