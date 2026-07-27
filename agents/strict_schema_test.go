package agents

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// These cases are ported from the Python SDK's test_strict_schema.py to keep the
// strict-mode conversion behavior aligned.

func TestStrict_EmptySchema(t *testing.T) {
	got, err := EnsureStrictJSONSchema(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", got["additionalProperties"])
	}
	if got["type"] != "object" {
		t.Errorf("type = %v", got["type"])
	}
}

func TestStrict_EmptySchemaFreshCopy(t *testing.T) {
	first, _ := EnsureStrictJSONSchema(map[string]any{})
	first["additionalProperties"] = true
	first["properties"].(map[string]any)["polluted"] = map[string]any{"type": "string"}

	second, _ := EnsureStrictJSONSchema(map[string]any{})
	if second["additionalProperties"] != false {
		t.Errorf("second copy polluted: %v", second["additionalProperties"])
	}
	if len(second["properties"].(map[string]any)) != 0 {
		t.Errorf("second properties polluted: %v", second["properties"])
	}
}

func TestStrict_ObjectWithoutAdditionalProperties(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	if got["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v", got["additionalProperties"])
	}
	if !reflect.DeepEqual(got["required"], []any{"a"}) {
		t.Errorf("required = %v, want [a]", got["required"])
	}
	if !reflect.DeepEqual(got["properties"].(map[string]any)["a"], map[string]any{"type": "string"}) {
		t.Errorf("inner property mutated: %v", got["properties"])
	}
}

func TestStrict_ObjectWithTrueAdditionalPropertiesErrors(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"a": map[string]any{"type": "number"}},
		"additionalProperties": true,
	}
	if _, err := EnsureStrictJSONSchema(schema); err == nil {
		t.Fatal("expected error for additionalProperties:true")
	}
}

func TestStrict_ArrayItemsAndDefaultRemoval(t *testing.T) {
	schema := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "number", "default": nil},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	items := got["items"].(map[string]any)
	if _, has := items["default"]; has {
		t.Errorf("default should be stripped: %v", items)
	}
	if items["type"] != "number" {
		t.Errorf("items type = %v", items["type"])
	}
}

func TestStrict_AnyOfProcessing(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			map[string]any{"type": "number", "default": nil},
		},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	variants := got["anyOf"].([]any)
	v0 := variants[0].(map[string]any)
	if v0["additionalProperties"] != false || !reflect.DeepEqual(v0["required"], []any{"a"}) {
		t.Errorf("variant0 = %v", v0)
	}
	v1 := variants[1].(map[string]any)
	if _, has := v1["default"]; has {
		t.Errorf("variant1 default not stripped: %v", v1)
	}
}

func TestStrict_OneOfFoldedToAnyOf(t *testing.T) {
	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := got["oneOf"]; has {
		t.Errorf("oneOf should be removed: %v", got)
	}
	if _, has := got["anyOf"]; !has {
		t.Errorf("anyOf should be present: %v", got)
	}
}

func TestStrict_AllOfSingleEntryMerging(t *testing.T) {
	schema := map[string]any{
		"type":  "object",
		"allOf": []any{map[string]any{"properties": map[string]any{"a": map[string]any{"type": "boolean"}}}},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := got["allOf"]; has {
		t.Errorf("allOf should be merged away: %v", got)
	}
	if got["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v", got["additionalProperties"])
	}
	if !reflect.DeepEqual(got["required"], []any{"a"}) {
		t.Errorf("required = %v", got["required"])
	}
}

func TestStrict_RefExpansion(t *testing.T) {
	schema := map[string]any{
		"definitions": map[string]any{"refObj": map[string]any{"type": "string", "default": nil}},
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/refObj", "description": "desc"}},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	a := got["properties"].(map[string]any)["a"].(map[string]any)
	if a["type"] != "string" {
		t.Errorf("a.type = %v, want string", a["type"])
	}
	if a["description"] != "desc" {
		t.Errorf("a.description = %v", a["description"])
	}
	if _, has := a["default"]; has {
		t.Errorf("a.default should be stripped: %v", a)
	}
}

func TestStrict_RefNoExpansionWhenAlone(t *testing.T) {
	schema := map[string]any{"$ref": "#/definitions/refObj"}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	if got["$ref"] != "#/definitions/refObj" || len(got) != 1 {
		t.Errorf("lone $ref should be untouched: %v", got)
	}
}

func TestStrict_InvalidRefFormat(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"$ref": "invalid", "description": "desc"}},
	}
	if _, err := EnsureStrictJSONSchema(schema); err == nil {
		t.Fatal("expected error for invalid $ref format")
	}
}

func TestStrict_ChainedRefWithSiblingKeys(t *testing.T) {
	schema := map[string]any{
		"$defs": map[string]any{
			"Inner": map[string]any{"type": "string"},
			"Outer": map[string]any{"$ref": "#/$defs/Inner"},
		},
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/Outer", "description": "desc"}},
	}
	got, err := EnsureStrictJSONSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	a := got["properties"].(map[string]any)["a"].(map[string]any)
	if a["type"] != "string" {
		t.Errorf("a.type = %v, want string (chain resolved)", a["type"])
	}
	if a["description"] != "desc" {
		t.Errorf("a.description = %v", a["description"])
	}
	if _, has := a["$ref"]; has {
		t.Errorf("a.$ref should be fully resolved: %v", a)
	}
}

// --- NewFunctionTool non-strict validation actually relaxes required keys.

func TestNewFunctionTool_NonStrictAllowsOmittedOptional(t *testing.T) {
	type args struct {
		City  string `json:"city" jsonschema:"the city"`
		Units string `json:"units,omitempty"`
	}
	tool := NewFunctionTool("get_weather", "",
		func(ctx context.Context, tc *ToolContext, a args) (string, error) {
			return a.City, nil
		})
	tc := &ToolContext{RunContext: NewRunContext(nil)}

	// Strict (default): the strict schema marks every field required, so omitting
	// the optional "units" is rejected as a missing-required-key error.
	if _, err := tool.OnInvoke(context.Background(), tc, `{"city":"Paris"}`); err == nil {
		t.Fatal("strict: expected missing-required-key error for omitted optional field")
	}

	// Non-strict: omitting the ",omitempty" field must now be accepted — before
	// the fix the closure kept validating against the all-required strict schema
	// and the relaxed tool was unusable.
	tool.Strict = false
	out, err := tool.OnInvoke(context.Background(), tc, `{"city":"Paris"}`)
	if err != nil {
		t.Fatalf("non-strict: omitted optional field should be accepted, got %v", err)
	}
	if out.ModelOutput() != "Paris" {
		t.Errorf("out = %v, want Paris", out)
	}

	// A genuinely required field is still enforced in non-strict mode.
	if _, err := tool.OnInvoke(context.Background(), tc, `{}`); err == nil {
		t.Fatal("non-strict: required field 'city' should still be enforced")
	}
}

// --- a typeless (map form) any property is rejected in strict mode.

func TestStrict_TypelessPropertyErrors(t *testing.T) {
	// An `any` field carrying a jsonschema description reflects to a typeless
	// {"description":...} property — not a boolean schema — which must still be
	// rejected in strict mode instead of slipping through to a 400.
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"data": map[string]any{"description": "some data"}},
	}
	if _, err := EnsureStrictJSONSchema(schema); err == nil {
		t.Fatal("expected error for typeless (unconstrained) property")
	}
}

func TestNewFunctionTool_TypelessAnyFieldFailsAtConstruction(t *testing.T) {
	type badArgs struct {
		Data any `json:"data" jsonschema:"some data"`
	}
	tool := NewFunctionTool("bad", "",
		func(ctx context.Context, tc *ToolContext, a badArgs) (string, error) {
			return "ran", nil
		})
	if tool.constructionErr == nil {
		t.Fatal("expected constructionErr for a tagged any field (typeless schema)")
	}
	if _, err := tool.OnInvoke(context.Background(), &ToolContext{}, `{"data":1}`); err == nil {
		t.Fatal("a broken-schema tool must error on invoke")
	}
}

func TestStrict_TypedAndCombinatorPropertiesStillValid(t *testing.T) {
	// Guard against false positives: properties with a type, a $ref, or a
	// combinator must not be flagged as unconstrained.
	schema := map[string]any{
		"$defs": map[string]any{"Inner": map[string]any{"type": "string"}},
		"type":  "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"$ref": "#/$defs/Inner", "description": "d"},
			"c": map[string]any{"anyOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "number"},
			}},
			"d": map[string]any{"enum": []any{"x", "y"}},
		},
	}
	if _, err := EnsureStrictJSONSchema(schema); err != nil {
		t.Fatalf("well-typed properties should not error: %v", err)
	}
}

// --- the unconstrained-schema error no longer recommends json.RawMessage.

func TestStrict_UnconstrainedErrorMessageOmitsRawMessage(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"data": true}, // untagged any -> boolean schema
	}
	_, err := EnsureStrictJSONSchema(schema)
	if err == nil {
		t.Fatal("expected error for boolean-schema property")
	}
	if strings.Contains(err.Error(), "json.RawMessage") {
		t.Errorf("error must not recommend json.RawMessage (it reflects to a byte array): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unconstrained") {
		t.Errorf("error should describe the unconstrained schema: %q", err.Error())
	}
}
