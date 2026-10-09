package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// thinkingOnTheWire builds a provider of the given type, applies the mode and
// the binding switch, and returns the "thinking" object of the request a run
// with an effort sends.
func thinkingOnTheWire(t *testing.T, providerType, mode string, binding bool) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		// The request is what is under test; the call may fail after it.
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"stop here"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	def, err := DefFor(providerType)
	if err != nil {
		t.Fatal(err)
	}
	p := ApplyThinking(def.Build("k", srv.URL, nil), mode, binding)
	m, err := p.Model("some-model")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = m.Respond(context.Background(), agents.ModelRequest{
		Input:    agents.InputItemsFromText("hi"),
		Settings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortMedium}},
	})
	if body == nil {
		t.Fatal("no request reached the endpoint")
	}
	thinking, _ := body["thinking"].(map[string]any)
	return thinking
}

// thinking_mode picks the form an Anthropic backend's reasoning effort takes,
// and the binding switch whether it asks for mismatched thinking to be dropped;
// another backend has neither and is left as it came.
func TestApplyThinking(t *testing.T) {
	got := thinkingOnTheWire(t, TypeAnthropic, "", true)
	if got["type"] != "adaptive" || got["block_binding"] == nil {
		t.Errorf("unset mode, binding on: thinking = %v, want adaptive with a block binding", got)
	}
	got = thinkingOnTheWire(t, TypeAnthropic, ThinkingModeBudget, false)
	if got["type"] != "enabled" || got["budget_tokens"] == nil || got["block_binding"] != nil {
		t.Errorf("budget mode, binding off: thinking = %v, want an enabled budget and no block binding", got)
	}
	if got := thinkingOnTheWire(t, TypeOpenAI, ThinkingModeBudget, true); got != nil {
		t.Errorf("an OpenAI request carries an Anthropic thinking object: %v", got)
	}
}
