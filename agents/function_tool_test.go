package agents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type weatherArgs struct {
	City  string `json:"city" jsonschema:"the city name"`
	Units string `json:"units,omitempty"`
}

func TestNewFunctionTool_SchemaGeneration(t *testing.T) {
	tool := NewFunctionTool("get_weather", "look up weather",
		func(ctx context.Context, tc *ToolContext, args weatherArgs) (string, error) {
			return "sunny in " + args.City, nil
		})

	if tool.Name != "get_weather" {
		t.Errorf("name = %q", tool.Name)
	}
	if !tool.Strict {
		t.Errorf("strict should default true")
	}
	schema := tool.ParamsJSONSchema
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false (strict)", schema["additionalProperties"])
	}
	// Strict mode: every property must be required, even json:omitempty ones.
	required, _ := schema["required"].([]any)
	if !reflect.DeepEqual(required, []any{"city", "units"}) {
		t.Errorf("required = %v, want [city units] (strict marks all)", required)
	}
	props := schema["properties"].(map[string]any)
	city := props["city"].(map[string]any)
	if city["description"] != "the city name" {
		t.Errorf("city description = %v (jsonschema tag)", city["description"])
	}
}

func TestNewFunctionTool_Invocation(t *testing.T) {
	tool := NewFunctionTool("get_weather", "look up weather",
		func(ctx context.Context, tc *ToolContext, args weatherArgs) (string, error) {
			if tc.ToolName != "get_weather" {
				t.Errorf("tool context name = %q", tc.ToolName)
			}
			return "sunny in " + args.City, nil
		})

	tc := &ToolContext{RunContext: NewRunContext(nil), ToolName: "get_weather"}
	out, err := tool.OnInvoke(context.Background(), tc, `{"city":"Shanghai","units":"c"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "sunny in Shanghai" {
		t.Errorf("output = %v", out)
	}
}

func TestNewFunctionTool_ArgumentValidation(t *testing.T) {
	tool := NewFunctionTool("t", "",
		func(ctx context.Context, tc *ToolContext, args weatherArgs) (string, error) {
			return "ran", nil
		})
	tc := &ToolContext{RunContext: NewRunContext(nil)}

	// Missing root-level required key: *ModelBehaviorError, generic wording.
	_, err := tool.OnInvoke(context.Background(), tc, `{"city":"Shanghai"}`)
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("missing key error = %v, want *ModelBehaviorError", err)
	}
	if msg := DefaultToolErrorFunction(context.Background(), tc, err); !strings.Contains(msg, "An error occurred while running the tool") {
		t.Errorf("missing-key message = %q, want the generic wording", msg)
	}

	// Undecodable JSON: *ModelBehaviorError with the dedicated parse wording.
	_, err = tool.OnInvoke(context.Background(), tc, `{not json`)
	if !errors.As(err, &mbe) {
		t.Fatalf("syntax error = %v, want *ModelBehaviorError", err)
	}
	if msg := DefaultToolErrorFunction(context.Background(), tc, err); !strings.Contains(msg, "An error occurred while parsing tool arguments. Please try again with valid JSON.") {
		t.Errorf("syntax message = %q, want the dedicated parse wording", msg)
	}

	// Non-object payload: *ModelBehaviorError, generic wording.
	_, err = tool.OnInvoke(context.Background(), tc, `[1,2]`)
	if !errors.As(err, &mbe) || !strings.Contains(err.Error(), "expected a JSON object") {
		t.Fatalf("non-object error = %v", err)
	}
}

func TestNewFunctionTool_EmptyArgsAllOptional(t *testing.T) {
	type noArgs struct{}
	tool := NewFunctionTool("t", "",
		func(ctx context.Context, tc *ToolContext, args noArgs) (string, error) {
			return "ok", nil
		})
	for _, in := range []string{"", "  \n", "{}"} {
		out, err := tool.OnInvoke(context.Background(), &ToolContext{}, in)
		if err != nil || out != "ok" {
			t.Errorf("input %q: out=%v err=%v", in, out, err)
		}
	}
}

func TestNewFunctionTool_InvalidArgs(t *testing.T) {
	tool := NewFunctionTool("t", "",
		func(ctx context.Context, tc *ToolContext, args weatherArgs) (string, error) {
			return "", nil
		})
	_, err := tool.OnInvoke(context.Background(), &ToolContext{}, `{"city": 123}`)
	if err == nil {
		t.Fatal("expected error for type-mismatched arguments")
	}
}

type extractResult struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestOutputType_Struct(t *testing.T) {
	schema := OutputType[extractResult]()
	if schema.IsPlainText() {
		t.Error("struct output should not be plain text")
	}
	if schema.Name() != "final_output" {
		t.Errorf("name = %q", schema.Name())
	}
	js := schema.JSONSchema()
	if js["type"] != "object" {
		t.Errorf("schema type = %v", js["type"])
	}

	v, err := schema.ValidateJSON(`{"name":"Ada","age":36}`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := v.(extractResult)
	if !ok {
		t.Fatalf("ValidateJSON returned %T, want extractResult", v)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("got %+v", got)
	}
}

func TestOutputType_WrappedSlice(t *testing.T) {
	schema := OutputType[[]string]()
	js := schema.JSONSchema()
	// Non-object types are wrapped so the root is an object.
	if js["type"] != "object" {
		t.Fatalf("wrapped schema type = %v", js["type"])
	}
	props := js["properties"].(map[string]any)
	if _, ok := props["response"]; !ok {
		t.Errorf("wrapped schema should have a 'response' property: %v", props)
	}

	v, err := schema.ValidateJSON(`{"response":["a","b"]}`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := v.([]string)
	if !ok {
		t.Fatalf("ValidateJSON returned %T, want []string", v)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
}

func TestOutputType_PlainText(t *testing.T) {
	schema := PlainTextOutput()
	if !schema.IsPlainText() {
		t.Error("should be plain text")
	}
	v, _ := schema.ValidateJSON("just text")
	if v != "just text" {
		t.Errorf("plain text validate = %v", v)
	}
}

func TestSchemaForType_RoundTripsToJSON(t *testing.T) {
	schema, err := SchemaFor[weatherArgs](true)
	if err != nil {
		t.Fatal(err)
	}
	// Must marshal cleanly (it is sent on the wire as tool parameters).
	if _, err := json.Marshal(schema); err != nil {
		t.Errorf("schema not JSON-marshalable: %v", err)
	}
}

// A tool built from an unusable schema (here an interface{} field, which
// has no strict-mode schema) fails the run with a *UserError before the model
// is ever called — not only when the model happens to invoke it.
func TestNewFunctionTool_BrokenSchemaFailsBeforeModelCall(t *testing.T) {
	type badArgs struct {
		Anything any `json:"anything"`
	}
	tool := NewFunctionTool("bad", "unusable",
		func(ctx context.Context, tc *ToolContext, args badArgs) (string, error) {
			return "ran", nil
		})
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{})
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *UserError for the broken tool schema", err)
	}
	if model.calls != 0 {
		t.Errorf("model was called %d times; a broken tool schema must fail before any model call", model.calls)
	}
}
