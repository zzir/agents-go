package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
	"github.com/zzir/agents-go/models/modelkit/conformancetest"
)

// TestConformance runs the shared adapter matrix against ResponsesModel,
// backed by a fake Responses API server. The fixtures are synthesized with the
// modelkit builders — for this provider the wire format IS the canonical
// format, so the test's value is proving the suite itself matches what the
// real adapter emits, keeping it honest for adapters that translate.
func TestConformance(t *testing.T) {
	conformancetest.Run(t, conformancetest.Target{
		NewModel: func(t *testing.T, s conformancetest.Scenario) agents.Model {
			t.Helper()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "responses") {
					http.NotFound(w, r)
					return
				}
				var body struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if body.Stream {
					writeConformanceStream(t, w, s.Turn)
				} else {
					writeConformanceResponse(t, w, s.Turn)
				}
			}))
			t.Cleanup(srv.Close)
			provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
			model, err := provider.Model("gpt-test")
			if err != nil {
				t.Fatal(err)
			}
			return model
		},
	})
}

// conformanceItems synthesizes the turn's output items in canonical form.
func conformanceItems(t *testing.T, turn conformancetest.TurnSpec) []agents.OutputItem {
	t.Helper()
	var items []agents.OutputItem
	add := func(item agents.OutputItem, err error) {
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	if turn.Refusal != "" {
		// The Responses API reports a refusal in-band, as the message itself;
		// there is no partial content beside it.
		add(modelkit.RefusalItem("msg_1", turn.Refusal))
		return items
	}
	if turn.Reasoning != nil {
		add(modelkit.ReasoningItem("rs_1", turn.Reasoning.Text, turn.Reasoning.Encrypted))
	}
	if turn.Text != "" {
		add(modelkit.MessageItem("msg_1", turn.Text))
	}
	for i, call := range turn.ToolCalls {
		add(modelkit.FunctionCallItem(fmt.Sprintf("fc_%d", i+1), call.CallID, call.Name, call.ArgumentsJSON))
	}
	return items
}

func conformanceUsage(turn conformancetest.TurnSpec) modelkit.ResponseUsage {
	return modelkit.ResponseUsage{
		InputTokens:      turn.Usage.Input,
		OutputTokens:     turn.Usage.Output,
		TotalTokens:      turn.Usage.Input + turn.Usage.Output,
		CachedTokens:     turn.Usage.CachedRead,
		CacheWriteTokens: turn.Usage.CacheWrite,
		ReasoningTokens:  turn.Usage.Reasoning,
	}
}

// writeConformanceResponse writes the blocking-path Response JSON.
func writeConformanceResponse(t *testing.T, w http.ResponseWriter, turn conformancetest.TurnSpec) {
	t.Helper()
	items := conformanceItems(t, turn)
	raws := make([]json.RawMessage, len(items))
	for i := range items {
		raws[i] = json.RawMessage(items[i].RawJSON())
	}
	usage := conformanceUsage(turn)
	resp := map[string]any{
		"id":     turn.ResponseID,
		"status": "completed",
		"output": raws,
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.TotalTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens":      usage.CachedTokens,
				"cache_write_tokens": usage.CacheWriteTokens,
			},
			"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
		},
	}
	if turn.Truncated {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Error(err)
	}
}

// writeConformanceStream writes the SSE event sequence for the turn.
func writeConformanceStream(t *testing.T, w http.ResponseWriter, turn conformancetest.TurnSpec) {
	t.Helper()
	items := conformanceItems(t, turn)
	var events []agents.ResponseStreamEvent
	add := func(ev agents.ResponseStreamEvent, err error) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}

	add(modelkit.ResponseCreatedEvent(turn.ResponseID))
	for i, item := range items {
		switch item.Type {
		case "message":
			add(modelkit.OutputItemAddedEvent(i, "message", item.ID, "", ""))
			if turn.Text != "" {
				for _, chunk := range splitInTwo(turn.Text) {
					add(modelkit.OutputTextDeltaEvent(item.ID, i, chunk))
				}
			}
		case "reasoning":
			add(modelkit.OutputItemAddedEvent(i, "reasoning", item.ID, "", ""))
			for _, chunk := range splitInTwo(turn.Reasoning.Text) {
				add(modelkit.ReasoningTextDeltaEvent(item.ID, i, chunk))
			}
		case "function_call":
			fc := item.AsFunctionCall()
			add(modelkit.OutputItemAddedEvent(i, "function_call", item.ID, fc.CallID, fc.Name))
			add(modelkit.FunctionCallArgumentsDeltaEvent(item.ID, i, fc.Arguments))
		}
		add(modelkit.OutputItemDoneEvent(i, item))
	}
	final := modelkit.FinalResponse{ID: turn.ResponseID, Output: items, Usage: conformanceUsage(turn)}
	if turn.Truncated {
		add(modelkit.IncompleteEvent(final, "max_output_tokens"))
	} else {
		add(modelkit.CompletedEvent(final))
	}

	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range events {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, ev.RawJSON())
	}
}

// splitInTwo cuts text into two deltas so the suite's concatenation check
// actually exercises reassembly.
func splitInTwo(text string) []string {
	if len(text) < 2 {
		return []string{text}
	}
	mid := len(text) / 2
	return []string{text[:mid], text[mid:]}
}
