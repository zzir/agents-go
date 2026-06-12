package agents

import (
	"context"
	"encoding/json"
	"reflect"
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
	out, err := tool.OnInvoke(context.Background(), tc, `{"city":"Shanghai"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "sunny in Shanghai" {
		t.Errorf("output = %v", out)
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
