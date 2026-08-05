package agents

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// wrapperDictKey is the property name used to wrap non-object output types, so
// the response schema root is always an object (an OpenAI requirement).
// ValidateJSON unwraps it, so the wrapping never reaches the caller.
const wrapperDictKey = "response"

// typedOutputSchema is the OutputSchema implementation backing OutputType[T].
type typedOutputSchema[T any] struct {
	schema    map[string]any
	wrapped   bool
	strict    bool
	typeName  string
	schemaErr error // deferred schema-generation failure, surfaced by the runner
	validator *schemaValidator
}

// OutputType returns an OutputSchema requesting structured output of type T.
//
// If T is object-like (a struct or map, optionally behind a pointer) the model
// is asked to produce that object directly. Otherwise (slices, primitives) the
// value is transparently wrapped in {"response": <value>} because OpenAI
// structured outputs require an object at the root; ValidateJSON unwraps it.
//
// Strict mode is enabled. Use OutputTypeNonStrict for a relaxed schema.
func OutputType[T any]() OutputSchema {
	return newOutputType[T](true)
}

// OutputTypeNonStrict is like OutputType but disables strict-mode schema
// normalization, allowing schema features OpenAI strict mode forbids.
func OutputTypeNonStrict[T any]() OutputSchema {
	return newOutputType[T](false)
}

func newOutputType[T any](strict bool) OutputSchema {
	t := reflect.TypeFor[T]()
	wrapped := !isObjectLike(t)

	var schema map[string]any
	var err error
	if wrapped {
		inner, e := schemaForType(t, false)
		err = e
		schema = map[string]any{
			"type":                 "object",
			"properties":           map[string]any{wrapperDictKey: inner},
			"required":             []any{wrapperDictKey},
			"additionalProperties": false,
		}
		if strict && err == nil {
			schema, err = EnsureStrictJSONSchema(schema)
		}
	} else {
		schema, err = schemaForType(t, strict)
	}

	s := &typedOutputSchema[T]{
		schema:    schema,
		wrapped:   wrapped,
		strict:    strict,
		typeName:  t.String(),
		validator: newSchemaValidator(schema),
	}
	if err != nil {
		// Defer surfacing the error: this keeps OutputType usable in a struct
		// literal without error handling. The runner checks schemaError before
		// the first model call so the broken schema is never sent to the API.
		s.schemaErr = NewUserError("output schema for %s: %v", t, err)
		s.schema = nil
	}
	return s
}

// schemaError exposes a deferred schema-generation failure to the runner.
func (s *typedOutputSchema[T]) schemaError() error { return s.schemaErr }

// wrappedSchema reports the {"response": ...} envelope to the runner, so a
// run-error handler's fallback output can be wrapped the same way the model's
// own output would be (see wrappedOutputSchema).
func (s *typedOutputSchema[T]) wrappedSchema() bool { return s.wrapped }

// outputSchemaError returns the deferred schema-generation failure of an
// OutputSchema, if it carries one.
func outputSchemaError(s OutputSchema) error {
	if c, ok := s.(interface{ schemaError() error }); ok {
		return c.schemaError()
	}
	return nil
}

// isObjectLike reports whether a type serializes to a JSON object at the root
// (a struct or map). Pointers are deliberately not unwrapped: a *T root would produce a
// nullable schema root, which OpenAI rejects, so pointer types go through the
// {"response": ...} envelope instead.
func isObjectLike(t reflect.Type) bool {
	return t.Kind() == reflect.Struct || t.Kind() == reflect.Map
}

func (s *typedOutputSchema[T]) IsPlainText() bool          { return false }
func (s *typedOutputSchema[T]) Name() string               { return "final_output" }
func (s *typedOutputSchema[T]) JSONSchema() map[string]any { return s.schema }
func (s *typedOutputSchema[T]) IsStrictJSONSchema() bool   { return s.strict }

// ValidateJSON parses the model's JSON output into a value of type T (unwrapping
// the {"response": ...} envelope when used).
func (s *typedOutputSchema[T]) ValidateJSON(jsonStr string) (any, error) {
	if s.schemaErr != nil {
		return nil, s.schemaErr
	}
	if s.wrapped {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(jsonStr), &probe); err != nil {
			return nil, fmt.Errorf("decoding wrapped output as %s: %w", s.typeName, err)
		}
		raw, ok := probe[wrapperDictKey]
		if !ok {
			return nil, fmt.Errorf("decoding wrapped output as %s: missing %q key", s.typeName, wrapperDictKey)
		}
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decoding wrapped output as %s: %w", s.typeName, err)
		}
		return v, nil
	}
	// encoding/json leaves a missing key at its zero value, so a model that
	// ignores the schema would otherwise decode into a silent zero rather than
	// an error the model can be asked to fix.
	//
	// This used to check root-level `required` by hand, which missed everything
	// nested: {"config":{"host":"x"}} passed a schema requiring config.port.
	// The validator checks the whole schema, and its messages carry a
	// JSON-pointer path — which is exactly what the model needs to correct
	// itself.
	if err := s.validator.Validate([]byte(jsonStr)); err != nil {
		return nil, fmt.Errorf("decoding output as %s: %w", s.typeName, err)
	}
	var v T
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return nil, fmt.Errorf("decoding output as %s: %w", s.typeName, err)
	}
	return v, nil
}

// plainTextSchema is the OutputSchema used when an agent has no OutputType; the
// model produces unstructured text.
type plainTextSchema struct{}

// PlainTextOutput returns the default OutputSchema representing free-form text.
func PlainTextOutput() OutputSchema { return plainTextSchema{} }

func (plainTextSchema) IsPlainText() bool                  { return true }
func (plainTextSchema) Name() string                       { return "final_output" }
func (plainTextSchema) JSONSchema() map[string]any         { return nil }
func (plainTextSchema) IsStrictJSONSchema() bool           { return true }
func (plainTextSchema) ValidateJSON(s string) (any, error) { return s, nil }
