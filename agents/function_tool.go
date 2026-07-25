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
// Strict mode is enabled by default: the generated schema marks every field
// required and forbids unknown properties, and the OpenAI API then guarantees
// the model sends every field. To relax it — so the model may omit fields whose
// json tag carries ",omitempty" — set Strict to false before use:
//
//	t:= agents.NewFunctionTool("get_weather", "look up weather", weatherFn)
//	t.Strict = false
//
// Setting Strict=false relaxes local argument validation automatically: a model
// call that omits an optional field is accepted instead of failing as a
// missing-required-key error. The schema advertised to the model, however, is
// still the strict-shaped one generated at construction (every field listed as
// required); to also relax what the model is told, replace ParamsJSONSchema with
// the non-strict schema, e.g.:
//
//	t.ParamsJSONSchema, _ = agents.SchemaFor[WeatherArgs](false)
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
	strictSchema, err := SchemaFor[A](true)
	if err != nil {
		// Schema generation only fails for unsupported types; surface it as a
		// tool that errors when invoked rather than panicking at construction.
		return failedFunctionTool(name, description,
			fmt.Errorf("function tool %q: schema generation failed: %w", name, err))
	}
	// The non-strict schema (optional fields not forced required) is what
	// argument validation must use once the caller disables strict mode;
	// otherwise the closure would keep enforcing the all-required strict schema
	// and reject any model call that omits an optional field, leaving the
	// relaxed tool unusable. Generated eagerly since strict generation already
	// succeeded; on the off chance it fails, fall back to the strict schema.
	nonStrictSchema, nerr := SchemaFor[A](false)
	if nerr != nil {
		nonStrictSchema = strictSchema
	}

	t := &FunctionTool{
		Name:                 name,
		Description:          description,
		ParamsJSONSchema:     strictSchema,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
	}
	t.OnInvoke = func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error) {
		// Validate against the schema matching the tool's *current* strictness,
		// read live so a post-construction t.Strict=false actually relaxes which
		// keys are required. In strict mode honor t.ParamsJSONSchema (defaults to
		// the strict schema, but respects a caller who replaced it); in
		// non-strict mode use the relaxed schema so omitted optional fields pass.
		validationSchema := t.ParamsJSONSchema
		if !t.Strict {
			validationSchema = nonStrictSchema
		}
		var args A
		if err := decodeToolArgs(name, validationSchema, argsJSON, &args); err != nil {
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
		OnInvoke: func(context.Context, *ToolContext, string) (ToolResult, error) {
			return ToolResult{}, err
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
// - undecodable JSON (syntax) — wrapped in toolArgumentsJSONError for the
// dedicated error wording,
// - a non-object payload,
// - a missing root-level required key (nested required fields are not
// enforced; see docs/migration_from_python.md),
// - a type mismatch while decoding into dst.
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
	fn func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error),
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
