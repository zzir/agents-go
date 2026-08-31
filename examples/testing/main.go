// Command testing shows how to test an agent with no model API key: fake the
// model, run everything else for real. Your tools actually execute; what is
// faked is only the decision to call them.
//
// The program itself runs against the scripted model, so it needs no key:
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

// scriptedModel returns one prepared response per turn. Model has two methods
// and a double only needs the one its callers reach: RunSync calls Respond.
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

// An output item is a Responses-API wire item; build one by encoding a value to
// JSON and decoding it back — encoding/json escapes every field, so no string
// concatenation hand-quotes the wire shape.
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

// "call the tool, then answer" is a two-response script: the first turn's
// function call makes the run execute the real tool, and its return value is
// fed to the second turn.
func callThenAnswer() *scriptedModel {
	return &scriptedModel{responses: []*agents.ModelResponse{
		{Output: []agents.OutputItem{functionCall("get_weather", "call_1", `{"city":"Beijing"}`)}},
		{Output: []agents.OutputItem{message("It is sunny and 21°C in Beijing.")}},
	}}
}

func main() {
	model := callThenAnswer()
	// Override replaces the model for every agent in the run, so a handoff
	// target gets the same script without being wired up separately.
	res, err := agents.RunSync(context.Background(), newAgent(), "weather in Beijing?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
	fmt.Printf("script consumed: %d/%d turns\n", model.calls, len(model.responses))
}
