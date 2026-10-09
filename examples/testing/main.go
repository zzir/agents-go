// Command testing tests an agent with no model API key: the model is scripted,
// everything else runs for real — tools actually execute; only the decision to
// call them is faked. See docs/howto/testing.md.
//
//	go run ./examples/testing     # runs the script
//	go test ./examples/testing    # asserts on it
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"

	"github.com/zzir/agents-go/agents"
)

// --- the agent under test ---

type weatherArgs struct {
	City string `json:"city" jsonschema_description:"The city to look up."`
}

func newAgent() *agents.Agent {
	weather := agents.NewTool("get_weather", "Look up the weather in a city.",
		func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
			return "sunny, 21°C in " + args.City, nil
		})
	return &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("Answer using get_weather when a city is mentioned."),
		Tools:        []*agents.Tool{weather},
	}
}

// --- the scripted model ---

// scriptedModel returns one prepared response per turn; RunSync reaches only Respond.
type scriptedModel struct {
	responses []*agents.ModelResponse
	calls     int
}

func (m *scriptedModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	if m.calls >= len(m.responses) {
		return nil, fmt.Errorf("script exhausted after %d turns", m.calls)
	}
	res := m.responses[m.calls]
	m.calls++
	return res, nil
}

func (m *scriptedModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	panic("this script only serves RunSync")
}

// outputItem builds a Responses wire item by a JSON round trip, so nothing is
// hand-quoted.
func outputItem(v any) agents.OutputItem {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("marshal output item: " + err.Error())
	}
	var item agents.OutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		panic("build output item: " + err.Error())
	}
	return item
}

func message(text string) agents.OutputItem {
	return outputItem(map[string]any{
		"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	})
}

func functionCall(name, callID, argsJSON string) agents.OutputItem {
	return outputItem(map[string]any{
		"type": "function_call", "id": "fc_1", "call_id": callID,
		"name": name, "arguments": argsJSON, "status": "completed",
	})
}

// callThenAnswer is a two-response script: the first turn's function call runs
// the real tool, and its return value feeds the second turn.
func callThenAnswer() *scriptedModel {
	return &scriptedModel{responses: []*agents.ModelResponse{
		{Output: []agents.OutputItem{functionCall("get_weather", "call_1", `{"city":"Beijing"}`)}},
		{Output: []agents.OutputItem{message("It is sunny and 21°C in Beijing.")}},
	}}
}

func main() {
	model := callThenAnswer()
	// Override replaces the model for every agent in the run, handoff targets included.
	res, err := agents.RunSync(context.Background(), newAgent(), "weather in Beijing?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
	fmt.Printf("script consumed: %d/%d turns\n", model.calls, len(model.responses))
}
