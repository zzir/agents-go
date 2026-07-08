package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// NewFunctionTool builds a FunctionTool from a typed Go function. The argument
// type A (which must be a struct) is reflected into a JSON Schema shown to the
// model; when the model calls the tool, the raw JSON arguments are unmarshaled
// into A and fn is invoked. The result R is returned to the model (serialized to
// JSON unless it is already a string).
//
// Strict mode is enabled by default. To disable it, or to set IsEnabled, mutate
// the returned tool's exported fields before use:
//
//	t := agents.NewFunctionTool("get_weather", "look up weather", weatherFn)
//	t.Strict = false
//
// This is the Go counterpart of Python's @function_tool decorator; reflection
// over struct tags replaces runtime signature inspection.
func NewFunctionTool[A any, R any](
	name, description string,
	fn func(ctx context.Context, tc *ToolContext, args A) (R, error),
) *FunctionTool {
	// Tool parameters must serialize to a JSON object, so A must be a struct
	// (or pointer to one). Rejecting other kinds here surfaces the mistake at
	// construction instead of a 400 from the API at request time.
	if argType := reflect.TypeFor[A](); !isStructKind(argType) {
		return failedFunctionTool(name, description,
			fmt.Errorf("function tool %q: args type %s is not a struct (or pointer to struct); tool parameters must be a JSON object", name, argType))
	}
	schema, err := SchemaFor[A](true)
	if err != nil {
		// Schema generation only fails for unsupported types; surface it as a
		// tool that errors when invoked rather than panicking at construction.
		return failedFunctionTool(name, description,
			fmt.Errorf("function tool %q: schema generation failed: %w", name, err))
	}

	return &FunctionTool{
		Name:                 name,
		Description:          description,
		ParamsJSONSchema:     schema,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		OnInvoke: func(ctx context.Context, tc *ToolContext, argsJSON string) (any, error) {
			var args A
			if err := decodeToolArgs(name, schema, argsJSON, &args); err != nil {
				return nil, err
			}
			return fn(ctx, tc, args)
		},
	}
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

// failedFunctionTool is the shared construction-failure channel: a tool whose
// schema (or argument type) is unusable is still returned as a valid value —
// keeping constructors chainable in struct literals — but reports err on every
// invocation instead of sending a broken schema to the API.
func failedFunctionTool(name, description string, err error) *FunctionTool {
	return &FunctionTool{
		Name:                 name,
		Description:          description,
		ParamsJSONSchema:     emptyStrictSchema(),
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		constructionErr:      err,
		OnInvoke: func(context.Context, *ToolContext, string) (any, error) {
			return nil, err
		},
	}
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
//   - undecodable JSON (syntax) — wrapped in toolArgumentsJSONError for the
//     dedicated error wording,
//   - a non-object payload,
//   - a missing root-level required key (nested required fields are not
//     enforced; see docs/python_differences.md),
//   - a type mismatch while decoding into dst.
//
// An empty or whitespace-only string is treated as "{}" so tools taking a
// struct with all-optional fields still work (Python parity: input_json or
// "{}").
func decodeToolArgs(toolName string, schema map[string]any, argsJSON string, dst any) error {
	trimmed := strings.TrimSpace(argsJSON)
	if trimmed == "" {
		trimmed = "{}"
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return &toolArgumentsJSONError{
			mbe:   newModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err),
			cause: err,
		}
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return newModelBehaviorError("Invalid JSON input for tool %s: expected a JSON object", toolName)
	}
	if required, rok := schema["required"].([]any); rok {
		for _, k := range required {
			key, _ := k.(string)
			if _, present := obj[key]; !present {
				return newModelBehaviorError("Invalid JSON input for tool %s: missing required key %q", toolName, key)
			}
		}
	}
	if err := json.Unmarshal([]byte(trimmed), dst); err != nil {
		return newModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	return nil
}

// NewRawFunctionTool builds a FunctionTool from a pre-built JSON Schema map and
// a function that receives raw JSON arguments. Use this when the schema is
// loaded at runtime (e.g. from a database) rather than derived from a Go type.
//
// Strict mode is enabled by default, and the schema is normalized to the
// strict subset via EnsureStrictJSONSchema — the same treatment Python's
// FunctionTool.__post_init__ applies — on a deep copy, so the caller's map is
// not mutated. If normalization fails (e.g. the schema contains features
// strict mode cannot express), the returned tool reports that error when
// invoked. To use the schema verbatim without strict mode, set Strict = false
// and ParamsJSONSchema on the returned tool.
func NewRawFunctionTool(
	name, description string,
	paramsSchema map[string]any,
	fn func(ctx context.Context, tc *ToolContext, argsJSON string) (any, error),
) *FunctionTool {
	normalized, err := ensureStrictSchemaCopy(paramsSchema)
	if err != nil {
		return failedFunctionTool(name, description,
			fmt.Errorf("raw function tool %q: strict schema normalization failed: %w", name, err))
	}
	return &FunctionTool{
		Name:                 name,
		Description:          description,
		ParamsJSONSchema:     normalized,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		OnInvoke:             fn,
	}
}
