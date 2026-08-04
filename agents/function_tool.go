package agents

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// NewFunctionTool builds a FunctionTool from a typed Go function. The argument
// type A (which must be a struct, or pointer to one) is reflected into a JSON
// Schema shown to the model; when the model calls the tool, the raw JSON
// arguments are validated against that schema, unmarshaled into A, and fn is
// invoked. The result R is returned to the model (serialized to JSON unless it
// is already a string).
//
// The schema is strict by default: every field is required and unknown
// properties are forbidden, and the OpenAI API then guarantees the model sends
// every field. Chain NonStrict to let the model omit fields whose json tag
// carries ",omitempty":
//
//	t := agents.NewFunctionTool("get_weather", "look up weather", weatherFn).NonStrict()
//
// NewFunctionTool panics when A cannot be reflected into a schema (not a
// struct, or a field of an unsupported type) — a deterministic programmer
// error, surfaced at construction like regexp.MustCompile. For a schema that
// is runtime data, use NewRawFunctionTool, which returns an error instead.
//
// This is the Go counterpart of Python's @function_tool decorator; reflection
// over struct tags replaces runtime signature inspection.
func NewFunctionTool[A any, R any](
	name, description string,
	fn func(ctx context.Context, tc *ToolContext, args A) (R, error),
) *FunctionTool {
	// Tool parameters must serialize to a JSON object, so A must be a struct
	// (or pointer to one).
	if argType := reflect.TypeFor[A](); !isStructKind(argType) {
		panic(fmt.Sprintf("agents: NewFunctionTool(%q): args type %s is not a struct (or pointer to struct); tool parameters must be a JSON object", name, argType))
	}
	regen := func(strict bool) (map[string]any, *schemaValidator) {
		schema, err := SchemaFor[A](strict)
		if err != nil {
			panic(fmt.Sprintf("agents: NewFunctionTool(%q): schema generation failed: %v", name, err))
		}
		// Compiled once per tool, not per call: a schema does not change
		// between turns.
		return schema, newSchemaValidator(schema)
	}
	t := &FunctionTool{
		Name:                 name,
		Description:          description,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		regen:                regen,
	}
	t.ParamsJSONSchema, t.validator = regen(true)
	t.OnInvoke = func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error) {
		var args A
		if err := decodeToolArgs(name, t.validator, argsJSON, &args); err != nil {
			return ToolResult{}, err
		}
		out, err := fn(ctx, tc, args)
		if err != nil {
			return ToolResult{}, err
		}
		// A tool that returns a ToolResult means it; anything else is wrapped,
		// so the ordinary `return "sunny", nil` keeps working.
		return resultFromValue(out), nil
	}
	return t
}

// isStructKind reports whether t is a struct or a pointer to a struct — the
// only argument shapes NewFunctionTool accepts, since tool parameters must be
// a JSON object.
func isStructKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// toolArgumentsJSONError marks tool arguments that were not decodable JSON at
// all (a syntax error, as opposed to a shape/validation mismatch), so
// DefaultToolErrorFunction can use Python's dedicated "parsing tool arguments"
// wording for it. It unwraps to a *ModelBehaviorError like every other
// argument failure.
type toolArgumentsJSONError struct {
	mbe   *ModelBehaviorError
	cause error // the underlying JSON syntax error
}

func (e *toolArgumentsJSONError) Error() string { return e.mbe.Error() }
func (e *toolArgumentsJSONError) Unwrap() error { return e.mbe }

// decodeToolArgs decodes and validates the model-provided JSON argument string
// into dst. Mirroring Python's _parse_function_tool_json_input + pydantic
// validation, every failure is a *ModelBehaviorError — fed back to the model
// via the tool's FailureErrorFunction so it can retry with corrected
// arguments:
// - undecodable JSON (syntax) — wrapped in toolArgumentsJSONError for the
// dedicated error wording,
// - a non-object payload,
// - anything the schema rejects, nested included,
// - a type mismatch while decoding into dst.
//
// An empty or whitespace-only string is treated as "{}" so tools taking a
// struct with all-optional fields still work (Python parity: input_json or
// "{}").
func decodeToolArgs(toolName string, v *schemaValidator, argsJSON string, dst any) error {
	trimmed := strings.TrimSpace(argsJSON)
	trimmed = cmp.Or(trimmed, "{}")
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return &toolArgumentsJSONError{
			mbe:   newModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err),
			cause: err,
		}
	}
	if _, ok := parsed.(map[string]any); !ok {
		return newModelBehaviorError("Invalid JSON input for tool %s: expected a JSON object", toolName)
	}
	// Fill in what the schema documents as a default before validating, so a
	// schema that advertises one and a tool that receives a zero value stop
	// telling two different stories.
	filled := v.ApplyDefaults([]byte(trimmed))
	// Validate the WHOLE schema. The hand-rolled check looked at root-level
	// required keys only, so a nested object missing a required field, or one
	// holding a string where the schema said integer, reached the tool as a
	// zero value it had no way to notice.
	if err := v.Validate(filled); err != nil {
		return newModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	if err := json.Unmarshal(filled, dst); err != nil {
		return newModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	return nil
}

// NewRawFunctionTool builds a FunctionTool from a pre-built JSON Schema map and
// a function that receives raw JSON arguments. Use this when the schema is
// loaded at runtime (e.g. from a database) rather than derived from a Go type —
// which is also why it returns an error where NewFunctionTool panics: a bad
// runtime schema is expected data, a bad argument type is a bug.
//
// Strict mode is enabled by default, and the schema is normalized to the
// strict subset via EnsureStrictJSONSchema — the same treatment Python's
// FunctionTool.__post_init__ applies — on a deep copy, so the caller's map is
// not mutated. To use the schema verbatim without strict mode, set
// ParamsJSONSchema and clear Strict on the returned tool.
func NewRawFunctionTool(
	name, description string,
	paramsSchema map[string]any,
	fn func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error),
) (*FunctionTool, error) {
	normalized, err := ensureStrictSchemaCopy(paramsSchema)
	if err != nil {
		return nil, fmt.Errorf("raw function tool %q: strict schema normalization failed: %w", name, err)
	}
	return &FunctionTool{
		Name:                 name,
		Description:          description,
		ParamsJSONSchema:     normalized,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		OnInvoke:             fn,
	}, nil
}
