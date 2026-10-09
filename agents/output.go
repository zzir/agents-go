package agents

import (
	"encoding/json"
	"fmt"
)

// OutputSchema describes the structured output a model is asked to produce;
// the adapter builds the response format from it and the runner validates with it.
type OutputSchema interface {
	// IsPlainText reports whether the output is unstructured text (no schema).
	IsPlainText() bool
	// Name is the schema name sent to the provider (e.g. "final_output").
	Name() string
	// JSONSchema returns the JSON Schema for the output object (non-plain-text only).
	JSONSchema() map[string]any
	// IsStrictJSONSchema reports whether strict-mode validation is requested.
	IsStrictJSONSchema() bool
	// ValidateJSON parses and validates the model's raw JSON output, returning
	// the decoded value.
	ValidateJSON(jsonStr string) (any, error)
}

// dynamicOutputSchema implements OutputSchema from a raw JSON Schema map loaded
// at runtime.
type dynamicOutputSchema struct {
	name      string
	schema    map[string]any
	strict    bool
	validator *schemaValidator
}

// NewDynamicOutputSchema returns an OutputSchema backed by a JSON Schema map;
// name is sent to the provider. strict normalizes a deep copy to the strict
// subset, and a schema strict mode cannot express is an error.
func NewDynamicOutputSchema(name string, schema map[string]any, strict bool) (OutputSchema, error) {
	s := &dynamicOutputSchema{name: name, schema: schema, strict: strict}
	if strict {
		normalized, err := ensureStrictSchemaCopy(schema)
		if err != nil {
			return nil, NewUserError("dynamic output schema %q: strict schema normalization failed: %v", name, err)
		}
		s.schema = normalized
	}
	s.validator = newSchemaValidator(s.schema)
	return s, nil
}

func (s *dynamicOutputSchema) IsPlainText() bool          { return false }
func (s *dynamicOutputSchema) Name() string               { return s.name }
func (s *dynamicOutputSchema) JSONSchema() map[string]any { return s.schema }
func (s *dynamicOutputSchema) IsStrictJSONSchema() bool   { return s.strict }
func (s *dynamicOutputSchema) ValidateJSON(raw string) (any, error) {
	// Validated locally too; the output is what the caller decodes.
	if err := s.validator.Validate([]byte(raw)); err != nil {
		return nil, fmt.Errorf("dynamic output schema %q: %w", s.name, err)
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("dynamic output schema %q: invalid JSON: %w", s.name, err)
	}
	return v, nil
}
