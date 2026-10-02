package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
)

// captured is what one request put on the wire.
type captured struct {
	beta string
	body map[string]any
}

// bindingServer answers every request with a short text reply and reports
// what the request carried; start is merged into message_start's message.
func bindingServer(t *testing.T, start map[string]any) (*httptest.Server, func() captured) {
	t.Helper()
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = captured{beta: strings.Join(r.Header.Values("anthropic-beta"), ",")}
		_ = json.Unmarshal(raw, &got.body)
		msg := map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
			"content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
		}
		for k, v := range start {
			msg[k] = v
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(eventType string, payload map[string]any) {
			data, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
		}
		send("message_start", map[string]any{"type": "message_start", "message": msg})
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}})
		send("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(srv.Close)
	return srv, func() captured { return got }
}

func bindingOf(c captured) any {
	thinking, _ := c.body["thinking"].(map[string]any)
	binding, _ := thinking["block_binding"].(map[string]any)
	return binding["prefix_mismatch_behavior"]
}

func withEffort() *agents.ModelSettings {
	return &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortMedium}}
}

// A request that enables thinking asks the API to drop a block bound to an
// edited prefix, under the beta the field needs — in either thinking form.
func TestThinkingRequestCarriesDropBlock(t *testing.T) {
	for name, budget := range map[string]bool{"adaptive": false, "budget": true} {
		srv, last := bindingServer(t, nil)
		m, err := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("k")).WithBudgetThinking(budget).Model("claude-test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Respond(context.Background(), agents.ModelRequest{Input: agents.InputItemsFromText("hi"), Settings: withEffort()}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := last(); bindingOf(got) != "drop_block" || got.beta != thinkingBindingBeta {
			t.Errorf("%s: binding = %v, anthropic-beta = %q; want drop_block under %s", name, bindingOf(got), got.beta, thinkingBindingBeta)
		}
	}
}

// A request with no thinking object gets neither: adding one for the binding's
// sake would turn thinking on.
func TestNoThinkingNoBinding(t *testing.T) {
	srv, last := bindingServer(t, nil)
	m, _ := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("k")).Model("claude-test")
	if _, err := m.Respond(context.Background(), agents.ModelRequest{Input: agents.InputItemsFromText("hi")}); err != nil {
		t.Fatal(err)
	}
	got := last()
	if _, has := got.body["thinking"]; has || got.beta != "" {
		t.Errorf("thinking = %v, anthropic-beta = %q; want neither", got.body["thinking"], got.beta)
	}
}

// The opt-out is for an endpoint that rejects the beta: thinking is still
// sent, the binding and its header are not.
func TestThinkingBindingOptOut(t *testing.T) {
	srv, last := bindingServer(t, nil)
	m, _ := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("k")).WithThinkingBinding(false).Model("claude-test")
	if _, err := m.Respond(context.Background(), agents.ModelRequest{Input: agents.InputItemsFromText("hi"), Settings: withEffort()}); err != nil {
		t.Fatal(err)
	}
	got := last()
	if thinking, _ := got.body["thinking"].(map[string]any); thinking["type"] != "adaptive" {
		t.Fatalf("thinking = %v, want it still sent", got.body["thinking"])
	}
	if bindingOf(got) != nil || got.beta != "" {
		t.Errorf("binding = %v, anthropic-beta = %q; want neither after the opt-out", bindingOf(got), got.beta)
	}
}

// A caller's own anthropic-beta survives: the binding's beta joins it.
func TestDropBlockKeepsCallerBetaHeader(t *testing.T) {
	srv, last := bindingServer(t, nil)
	m, _ := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("k")).Model("claude-test")
	settings := withEffort()
	settings.ExtraHeaders = map[string]string{"Anthropic-Beta": "some-other-beta-2026-01-01"}
	if _, err := m.Respond(context.Background(), agents.ModelRequest{Input: agents.InputItemsFromText("hi"), Settings: settings}); err != nil {
		t.Fatal(err)
	}
	got := last()
	if !strings.Contains(got.beta, "some-other-beta-2026-01-01") || !strings.Contains(got.beta, thinkingBindingBeta) {
		t.Errorf("anthropic-beta = %q, want the caller's value and the binding's", got.beta)
	}
	if bindingOf(got) != "drop_block" {
		t.Errorf("binding = %v, want drop_block", bindingOf(got))
	}
}

// What the API dropped is a diagnostic of the run: each thinking_dropped entry
// of message_start, with its path and reason. Other entries are not drops.
func TestInputTransformationsBecomeDiagnostics(t *testing.T) {
	srv, _ := bindingServer(t, map[string]any{"input_transformations": []any{
		map[string]any{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_binding_mismatch"},
		map[string]any{"type": "thinking_mismatch_allowed", "path": "messages.3.content.0", "reason": "prefix_binding_mismatch"},
	}})
	agent := &agents.Agent{Name: "a", Model: "claude-test", ModelSettings: withEffort()}
	res, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{
		Model: agents.ModelOptions{Provider: NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("k"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	var dropped []agents.Diagnostic
	for _, d := range res.Diagnostics {
		if d.Type == diagnosticThinkingDropped {
			dropped = append(dropped, d)
		}
	}
	if len(dropped) != 1 || dropped[0].Details["path"] != "messages.1.content.0" || dropped[0].Details["reason"] != "prefix_binding_mismatch" {
		t.Fatalf("diagnostics = %+v, want the one dropped block with its path and reason", res.Diagnostics)
	}
}
