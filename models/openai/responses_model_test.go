package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// fakeSchema is a minimal OutputSchema for exercising response_format building
// before the reflection-based implementation lands in Phase 2.
type fakeSchema struct {
	plain  bool
	strict bool
}

func (f fakeSchema) IsPlainText() bool        { return f.plain }
func (f fakeSchema) Name() string             { return "final_output" }
func (f fakeSchema) IsStrictJSONSchema() bool { return f.strict }
func (f fakeSchema) JSONSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (f fakeSchema) ValidateJSON(string) (any, error) { return nil, nil }

func marshalParams(t *testing.T, req agents.ModelRequest) map[string]any {
	t.Helper()
	m := NewResponsesModel("gpt-4o", NewProvider().client.Responses)
	params, err := m.buildParams(req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestBuildParams_BasicTextInput(t *testing.T) {
	req := agents.ModelRequest{
		SystemInstructions: "be helpful",
		Input:              agents.InputItemsFromText("hello"),
	}
	got := marshalParams(t, req)

	if got["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", got["model"])
	}
	if got["instructions"] != "be helpful" {
		t.Errorf("instructions = %v", got["instructions"])
	}
	if _, ok := got["tools"]; ok {
		t.Errorf("tools should be omitted when none provided, got %v", got["tools"])
	}
}

func TestBuildParams_FunctionTool(t *testing.T) {
	tool := &agents.FunctionTool{
		Name:        "get_weather",
		Description: "look up weather",
		Strict:      true,
		ParamsJSONSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"city": map[string]any{"type": "string"}},
			"required":             []any{"city"},
			"additionalProperties": false,
		},
	}
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("weather in SF?"),
		Tools: []agents.Tool{tool},
	}
	got := marshalParams(t, req)

	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", got["tools"])
	}
	ft := tools[0].(map[string]any)
	if ft["name"] != "get_weather" {
		t.Errorf("tool name = %v", ft["name"])
	}
	if ft["type"] != "function" {
		t.Errorf("tool type = %v", ft["type"])
	}
	if ft["strict"] != true {
		t.Errorf("tool strict = %v, want true", ft["strict"])
	}
	if ft["description"] != "look up weather" {
		t.Errorf("tool description = %v", ft["description"])
	}
}

func TestBuildParams_StructuredOutput(t *testing.T) {
	req := agents.ModelRequest{
		Input:        agents.InputItemsFromText("extract"),
		OutputSchema: fakeSchema{plain: false, strict: true},
	}
	got := marshalParams(t, req)

	text, ok := got["text"].(map[string]any)
	if !ok {
		t.Fatalf("text config missing: %v", got["text"])
	}
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("format type = %v", format["type"])
	}
	if format["name"] != "final_output" {
		t.Errorf("format name = %v", format["name"])
	}
	if format["strict"] != true {
		t.Errorf("format strict = %v", format["strict"])
	}
}

func TestBuildParams_PlainTextOutputOmitsFormat(t *testing.T) {
	req := agents.ModelRequest{
		Input:        agents.InputItemsFromText("hi"),
		OutputSchema: fakeSchema{plain: true},
	}
	got := marshalParams(t, req)
	if _, ok := got["text"]; ok {
		t.Errorf("text config should be omitted for plain text, got %v", got["text"])
	}
}

func TestBuildParams_Settings(t *testing.T) {
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("hi"),
		Tools: []agents.Tool{&agents.FunctionTool{Name: "t", ParamsJSONSchema: map[string]any{"type": "object"}}},
		Settings: &agents.ModelSettings{
			Temperature:       agents.Ptr(0.5),
			MaxTokens:         agents.Ptr[int64](256),
			ToolChoice:        agents.ToolChoiceRequired,
			ParallelToolCalls: agents.Ptr(true),
		},
	}
	got := marshalParams(t, req)

	if got["temperature"] != 0.5 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	if got["max_output_tokens"] != float64(256) {
		t.Errorf("max_output_tokens = %v", got["max_output_tokens"])
	}
	if got["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v", got["tool_choice"])
	}
	if got["parallel_tool_calls"] != true {
		t.Errorf("parallel_tool_calls = %v", got["parallel_tool_calls"])
	}
}

type integArgs struct {
	City string `json:"city" jsonschema:"city name"`
}

func TestBuildParams_GeneratedFunctionToolSchema(t *testing.T) {
	tool := agents.NewFunctionTool("get_weather", "look up weather",
		func(ctx context.Context, tc *agents.ToolContext, a integArgs) (string, error) {
			return "", nil
		})
	req := agents.ModelRequest{
		Input: agents.InputItemsFromText("weather?"),
		Tools: []agents.Tool{tool},
	}
	got := marshalParams(t, req)
	tools := got["tools"].([]any)
	ft := tools[0].(map[string]any)
	params := ft["parameters"].(map[string]any)
	if params["additionalProperties"] != false {
		t.Errorf("generated schema not strict: %v", params)
	}
	if ft["strict"] != true {
		t.Errorf("strict = %v", ft["strict"])
	}
}
