package agents

import (
	"encoding/json"
	"fmt"
)

// OutputSchema describes the structured output a model is asked to produce. It
// is the Go counterpart of the Python SDK's AgentOutputSchemaBase abstract base
// class. The OpenAI model adapter uses it to build the response_format payload
// and to validate/parse the model's final output.
//
// The full reflection-based implementation lands in Phase 2; this interface is
// defined here because the Model interface depends on it.
type OutputSchema interface {
	// IsPlainText reports whether the output is unstructured text (no schema).
	IsPlainText() bool
	// Name is the schema name sent to the provider (e.g. "final_output").
	Name() string
	// JSONSchema returns the JSON Schema for the output object. It is only
	// called when IsPlainText reports false.
	JSONSchema() map[string]any
	// IsStrictJSONSchema reports whether strict-mode validation is requested.
	IsStrictJSONSchema() bool
	// ValidateJSON parses and validates a raw JSON string produced by the model,
	// returning the decoded Go value.
	ValidateJSON(jsonStr string) (any, error)
}

// dynamicOutputSchema implements OutputSchema from a raw JSON Schema map,
// for use cases where the schema is loaded at runtime (e.g. from a database)
// rather than derived from a Go type at compile time.
type dynamicOutputSchema struct {
	name   string
	schema map[string]any
	strict bool
}

// NewDynamicOutputSchema returns an OutputSchema backed by the given JSON Schema
// map. name is the schema identifier sent to the provider (e.g. "final_output").
// When strict is true, IsStrictJSONSchema reports true.
func NewDynamicOutputSchema(name string, schema map[string]any, strict bool) OutputSchema {
	return &dynamicOutputSchema{name: name, schema: schema, strict: strict}
}

func (s *dynamicOutputSchema) IsPlainText() bool          { return false }
func (s *dynamicOutputSchema) Name() string               { return s.name }
func (s *dynamicOutputSchema) JSONSchema() map[string]any { return s.schema }
func (s *dynamicOutputSchema) IsStrictJSONSchema() bool   { return s.strict }
func (s *dynamicOutputSchema) ValidateJSON(raw string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("dynamic output schema %q: invalid JSON: %w", s.name, err)
	}
	return v, nil
}
