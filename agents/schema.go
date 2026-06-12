package agents

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaForType generates a JSON Schema (as a map) for the given Go type using
// reflection. Struct fields are named by their json tag and described by a
// "jsonschema" struct tag (the whole tag value is the field description). The
// schema is inlined (no $ref/$defs) and the root is the type's own object
// schema, which is the shape OpenAI tool/output schemas require.
//
// When strict is true the schema is additionally rewritten to satisfy OpenAI
// strict mode via EnsureStrictJSONSchema.
func schemaForType(t reflect.Type, strict bool) (map[string]any, error) {
	reflected, err := jsonschema.ForType(t, nil)
	if err != nil {
		return nil, fmt.Errorf("generating schema for %s: %w", t, err)
	}
	raw, err := json.Marshal(reflected)
	if err != nil {
		return nil, fmt.Errorf("marshaling schema for %s: %w", t, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decoding schema for %s: %w", t, err)
	}
	// These keys are not meaningful to the OpenAI schema validators.
	delete(m, "$schema")
	delete(m, "$id")
	delete(m, "$defs")
	delete(m, "definitions")

	// OpenAI requires object schemas to declare "properties" even when empty.
	if m["type"] == "object" {
		if _, ok := m["properties"]; !ok {
			m["properties"] = map[string]any{}
		}
	}

	if strict {
		return EnsureStrictJSONSchema(m)
	}
	return m, nil
}

// SchemaFor returns the JSON Schema map for type T, applying strict-mode
// normalization. It is exported for callers who want to inspect or reuse the
// schema the SDK would generate for a tool or output type.
func SchemaFor[T any](strict bool) (map[string]any, error) {
	return schemaForType(reflect.TypeFor[T](), strict)
}
