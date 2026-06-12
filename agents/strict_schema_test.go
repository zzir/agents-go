package agents

import (
	"reflect"
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
