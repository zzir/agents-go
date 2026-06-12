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
	schema, err := SchemaFor[A](true)
	if err != nil {
		// Schema generation only fails for pathological types; surface it as a
		// tool that errors when invoked rather than panicking at construction.
		return &FunctionTool{
			Name:                 name,
			Description:          description,
			ParamsJSONSchema:     emptyStrictSchema(),
			Strict:               true,
			FailureErrorFunction: DefaultToolErrorFunction,
			OnInvoke: func(context.Context, *ToolContext, string) (any, error) {
				return nil, fmt.Errorf("function tool %q: schema generation failed: %w", name, err)
			},
		}
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
