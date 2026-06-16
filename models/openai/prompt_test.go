package openai

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestConvertPrompt(t *testing.T) {
	p := convertPrompt(&agents.Prompt{
		ID:        "pmpt_123",
		Version:   "2",
		Variables: map[string]any{"city": "Paris", "n": 7},
	})
	if p.ID != "pmpt_123" {
		t.Errorf("ID = %q", p.ID)
	}
	if p.Version.Value != "2" {
		t.Errorf("Version = %q", p.Version.Value)
	}
	if got := p.Variables["city"].OfString.Value; got != "Paris" {
		t.Errorf("city = %q", got)
	}
	// Non-string variables are stringified.
	if got := p.Variables["n"].OfString.Value; got != "7" {
		t.Errorf("n = %q", got)
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
