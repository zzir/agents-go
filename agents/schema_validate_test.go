package agents

import (
	"context"
	"strings"
	"testing"
)

type netConfig struct {
	Host string `json:"host" jsonschema:"hostname"`
	Port int    `json:"port" jsonschema:"port number"`
}

type deployArgs struct {
	Name   string    `json:"name" jsonschema:"deployment name"`
	Config netConfig `json:"config" jsonschema:"network config"`
}

// The gap the hand-rolled check had: it looked at root-level required keys
// only, so a nested object missing a required field reached the tool as a zero
// value it had no way to notice.
func TestSchemaValidation_CatchesNestedRequired(t *testing.T) {
	var got deployArgs
	tool := NewFunctionTool("deploy", "", func(_ context.Context, _ *ToolContext, a deployArgs) (string, error) {
		got = a
		return "deployed", nil
	})
	tool.FailureErrorFunction = nil // surface the rejection instead of feeding it back

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "deploy", "c1", `{"name":"web","config":{"host":"h"}}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatalf("a nested missing field was accepted; the tool saw %+v", got)
	}
	// The message carries a JSON-pointer path, which is what the model needs to
	// correct itself.
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want it to name the missing nested field", err)
	}
}

func TestSchemaValidation_CatchesNestedTypeMismatch(t *testing.T) {
	tool := NewFunctionTool("deploy", "", func(context.Context, *ToolContext, deployArgs) (string, error) {
		return "deployed", nil
	})
	tool.FailureErrorFunction = nil
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "deploy", "c1", `{"name":"web","config":{"host":"h","port":"not-an-int"}}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err == nil {
		t.Fatal("a nested type mismatch was accepted")
	}
}

// An unexpected key is dropped by Go decoding and the tool cannot see it, so
// rejecting the call would turn a harmless extra into a failed turn. A
// misspelled key is still caught, by `required`.
func TestSchemaValidation_ExtraKeysAreHarmless(t *testing.T) {
	tool := NewFunctionTool("noop", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "noop", "c1", `{"unexpected":"value"}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatalf("an extra property failed the call: %v", err)
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// A required key sent under the wrong name is still rejected — that is what
// `required` is for.
func TestSchemaValidation_WrongKeyNameIsCaught(t *testing.T) {
	tool := NewFunctionTool("deploy", "", func(context.Context, *ToolContext, deployArgs) (string, error) {
		return "deployed", nil
	})
	tool.FailureErrorFunction = nil
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "deploy", "c1", `{"deployment":"web","config":{"host":"h","port":1}}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err == nil {
		t.Fatal("a required key sent under the wrong name was accepted")
	}
}

// A structured output whose nested object ignores the schema used to decode
// into a silent zero value.
func TestSchemaValidation_StructuredOutputChecksNested(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, `{"name":"web","config":{"host":"h"}}`)),
	}}
	agent := &Agent{Name: "a", OutputType: OutputType[deployArgs](), ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("a nested missing field was accepted as structured output")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want it to name the missing nested field", err)
	}
}

// A runtime-loaded schema was checked for nothing beyond "is this JSON".
func TestSchemaValidation_DynamicSchemaIsEnforced(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
		"required": []any{"count"},
	}
	out := NewDynamicOutputSchema("result", schema, false)

	if _, err := out.ValidateJSON(`{"count":"nope"}`); err == nil {
		t.Error("a type mismatch was accepted by a dynamic schema")
	}
	if _, err := out.ValidateJSON(`{}`); err == nil {
		t.Error("a missing required key was accepted by a dynamic schema")
	}
	if _, err := out.ValidateJSON(`{"count":3}`); err != nil {
		t.Errorf("a valid instance was rejected: %v", err)
	}
}

// A schema that documents a default and a tool that receives a zero value are
// telling two different stories.
func TestSchemaValidation_AppliesDefaults(t *testing.T) {
	v := newSchemaValidator(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string", "default": "dflt"},
			"b": map[string]any{"type": "integer"},
		},
	})
	out := v.ApplyDefaults([]byte(`{"b":7}`))
	if !strings.Contains(string(out), `"dflt"`) {
		t.Errorf("defaults = %s, want the schema's default applied", out)
	}
}

// A schema this SDK cannot compile may still be one the provider understands;
// refusing to run would turn a missing local check into a broken feature.
func TestSchemaValidation_UncompilableSchemaSkipsValidation(t *testing.T) {
	v := newSchemaValidator(map[string]any{"type": func() {}})
	if err := v.Validate([]byte(`{"anything":1}`)); err != nil {
		t.Errorf("an uncompilable schema failed the call: %v", err)
	}
}
