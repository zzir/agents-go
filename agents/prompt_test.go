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
	if p, err := a.GetPrompt(ctx, rc); err != nil || p != nil {
		t.Fatalf("nil prompt: p=%v err=%v", p, err)
	}

	// static prompt
	a.Prompt = StaticPrompt(Prompt{ID: "pmpt_1", Version: "3"})
	p, err := a.GetPrompt(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "pmpt_1" || p.Version != "3" {
		t.Errorf("static prompt = %+v", p)
	}

	// dynamic prompt
	a.Prompt = PromptFunc(func(context.Context, *RunContext, *Agent) (*Prompt, error) {
		return &Prompt{ID: "pmpt_dyn"}, nil
	})
	if p, _ := a.GetPrompt(ctx, rc); p.ID != "pmpt_dyn" {
		t.Errorf("dynamic prompt = %+v", p)
	}

	// missing ID -> error
	a.Prompt = StaticPrompt(Prompt{Version: "1"})
	if _, err := a.GetPrompt(ctx, rc); err == nil {
		t.Error("prompt without ID should error")
	}
}
