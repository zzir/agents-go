package agents

import (
	"context"
	"testing"
)

func TestAgentGetPrompt(t *testing.T) {
	ctx := context.Background()
	rc := &RunContext{}

	// nil prompt -> nil, no error
	a := &Agent{Name: "a"}
	if p, err := a.resolvePrompt(ctx, rc); err != nil || p != nil {
		t.Fatalf("nil prompt: p=%v err=%v", p, err)
	}

	// static prompt
	a.Prompt = StaticPrompt(Prompt{ID: "pmpt_1", Version: "3"})
	p, err := a.resolvePrompt(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "pmpt_1" || p.Version != "3" {
		t.Errorf("static prompt = %+v", p)
	}

	// dynamic prompt
	a.Prompt = func(context.Context, *RunContext, *Agent) (*Prompt, error) {
		return &Prompt{ID: "pmpt_dyn"}, nil
	}
	if p, _ := a.resolvePrompt(ctx, rc); p.ID != "pmpt_dyn" {
		t.Errorf("dynamic prompt = %+v", p)
	}

	// missing ID -> error
	a.Prompt = StaticPrompt(Prompt{Version: "1"})
	if _, err := a.resolvePrompt(ctx, rc); err == nil {
		t.Error("prompt without ID should error")
	}
}

func TestStaticPromptIsolatesVariables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := &Agent{
		Name:   "a",
		Prompt: StaticPrompt(Prompt{ID: "pmpt_1", Variables: map[string]any{"city": "Paris"}}),
	}

	first, err := a.resolvePrompt(ctx, &RunContext{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.resolvePrompt(ctx, &RunContext{})
	if err != nil {
		t.Fatal(err)
	}

	first.Variables["city"] = "Berlin"
	if second.Variables["city"] != "Paris" {
		t.Errorf("second run saw %v, want Paris: StaticPrompt must not share the variable map across runs", second.Variables["city"])
	}
}
