package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
			if err := unmarshalToolArgs(argsJSON, &args); err != nil {
				return nil, fmt.Errorf("function tool %q: invalid arguments: %w", name, err)
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
		OnInvoke: func(context.Context, *ToolContext, string) (any, error) {
			return nil, err
		},
	}
}

// unmarshalToolArgs decodes the model-provided JSON argument string into dst.
// An empty or whitespace string is treated as an empty object so that tools
// taking a struct with all-optional fields still work.
func unmarshalToolArgs(argsJSON string, dst any) error {
	trimmed := argsJSON
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\n' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if trimmed == "" {
		// Nothing to decode; leave dst at its zero value.
		if reflect.TypeOf(dst).Elem().Kind() == reflect.Struct {
			return nil
		}
	}
	return json.Unmarshal([]byte(argsJSON), dst)
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
