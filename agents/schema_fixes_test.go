package agents

import (
	"context"
	"strings"
	"testing"
)

// OpenAI strict mode rejects object schemas without "properties"; empty
// argument structs are the common case.
func TestSchemaForEmptyStructHasProperties(t *testing.T) {
	schema, err := SchemaFor[struct{}](true)
	if err != nil {
		t.Fatal(err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema missing properties: %v", schema)
	}
	if len(props) != 0 {
		t.Errorf("properties = %v, want empty", props)
	}
	if _, ok := schema["required"]; !ok {
		t.Errorf("schema missing required: %v", schema)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
}

// A pointer output type must go through the {"response": ...} envelope: a
// nullable schema root would be rejected by the API.
func TestOutputTypePointerIsWrapped(t *testing.T) {
	type inner struct {
		Name string `json:"name"`
	}
	s := OutputType[*inner]()
	if err := outputSchemaError(s); err != nil {
		t.Fatal(err)
	}
	schema := s.JSONSchema()
	if schema["type"] != "object" {
		t.Errorf("root type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props[wrapperDictKey]; !ok {
		t.Fatalf("expected %q envelope, got %v", wrapperDictKey, schema)
	}
	v, err := s.ValidateJSON(`{"response":{"name":"x"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := v.(*inner); !ok || p == nil || p.Name != "x" {
		t.Errorf("validated = %#v", v)
	}
	// A wrapped payload without the envelope key is a model error, not a zero value.
	if _, err := s.ValidateJSON(`{"wrong":1}`); err == nil {
		t.Error("expected error for missing response key")
	}
}

// Schema generation failures must fail the run before any model call instead
// of sending a bogus schema to the API.
func TestRun_OutputSchemaErrorSurfaces(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, `{}`))}}
	agent := &Agent{
		Name:       "a",
		ModelImpl:  model,
		OutputType: OutputType[map[string]string](), // additionalProperties: schema → strict failure
	}
	_, err := Run(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("expected schema error to fail the run")
	}
	if !strings.Contains(err.Error(), "output schema") {
		t.Errorf("err = %v", err)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times, want 0", model.calls)
	}
}
