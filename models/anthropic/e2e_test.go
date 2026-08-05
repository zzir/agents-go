package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
)

// TestRunSyncToolLoop drives the real run loop through the adapter: turn one
// answers with tool_use, the runner executes the tool and resends, and the
// test asserts the SECOND request's wire body carries the assistant tool_use
// and the user tool_result — the request direction the conformance suite
// cannot see.
func TestRunSyncToolLoop(t *testing.T) {
	var calls atomic.Int64
	var secondRequest []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var reply string
		switch calls.Add(1) {
		case 1:
			reply = `{
				"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
				"content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Oslo"}}],
				"stop_reason": "tool_use",
				"usage": {"input_tokens": 10, "output_tokens": 5}
			}`
		default:
			var body struct {
				Messages json.RawMessage `json:"messages"`
			}
			raw := readBody(t, r)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Error(err)
			}
			secondRequest = body.Messages
			reply = `{
				"id": "msg_2", "type": "message", "role": "assistant", "model": "claude-test",
				"content": [{"type": "text", "text": "Sunny in Oslo."}],
				"stop_reason": "end_turn",
				"usage": {"input_tokens": 30, "output_tokens": 6}
			}`
		}
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)

	type weatherArgs struct {
		City string `json:"city"`
	}
	var toolCity string
	getWeather := agents.NewFunctionTool("get_weather", "Look up the weather.",
		func(_ context.Context, _ *agents.ToolContext, args weatherArgs) (string, error) {
			toolCity = args.City
			return "sunny", nil
		})

	agent := &agents.Agent{
		Name:  "weather-bot",
		Model: "claude-test",
		Tools: []*agents.FunctionTool{getWeather},
	}
	res, err := agents.RunSync(context.Background(), agent, "Weather in Oslo?", agents.RunOptions{
		Model: agents.ModelOptions{
			Provider: NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.FinalOutputString(); got != "Sunny in Oslo." {
		t.Errorf("final output = %q", got)
	}
	if toolCity != "Oslo" {
		t.Errorf("tool args city = %q — the tool_use input must reach the tool decoder", toolCity)
	}
	if calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2", calls.Load())
	}

	wire := string(secondRequest)
	if !strings.Contains(wire, `"tool_use"`) || !strings.Contains(wire, `"toolu_1"`) {
		t.Errorf("second request lacks the assistant tool_use turn: %s", wire)
	}
	if !strings.Contains(wire, `"tool_result"`) || !strings.Contains(wire, "sunny") {
		t.Errorf("second request lacks the tool_result turn: %s", wire)
	}
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
