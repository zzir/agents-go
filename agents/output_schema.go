package agents

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// wrapperDictKey wraps a non-object output type so the schema root is an
// object (an OpenAI requirement); ValidateJSON unwraps it.
const wrapperDictKey = "response"

// typedOutputSchema is the OutputSchema implementation backing OutputType[T].
type typedOutputSchema[T any] struct {
	schema    map[string]any
	wrapped   bool
	strict    bool
	typeName  string
	validator *schemaValidator
}

// OutputType returns a strict OutputSchema for type T; a non-object T is
// transparently wrapped in {"response": <value>}. A T strict mode cannot
// express panics, as NewTool does (decisions §5.11); OutputTypeNonStrict relaxes it.
func OutputType[T any]() OutputSchema {
	return newOutputType[T]("OutputType", true)
}

// OutputTypeNonStrict is like OutputType but disables strict-mode schema
// normalization, allowing schema features OpenAI strict mode forbids.
func OutputTypeNonStrict[T any]() OutputSchema {
	return newOutputType[T]("OutputTypeNonStrict", false)
}

func newOutputType[T any](ctor string, strict bool) OutputSchema {
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

	if err != nil {
		panic(fmt.Sprintf("agents: %s[%s]: schema generation failed: %v", ctor, t, err))
	}
	return &typedOutputSchema[T]{
		schema:    schema,
		wrapped:   wrapped,
		strict:    strict,
		typeName:  t.String(),
		validator: newSchemaValidator(schema),
	}
}

// wrappedSchema reports whether output sits inside the {"response": ...} envelope
// (see wrappedOutputSchema).
func (s *typedOutputSchema[T]) wrappedSchema() bool { return s.wrapped }

// isObjectLike reports whether a type serializes to a JSON object at the root (a
// struct or map); pointers are not unwrapped, as a nullable root is rejected.
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
	// Validate the whole schema first: encoding/json would leave a missing key
	// at its zero value.
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
