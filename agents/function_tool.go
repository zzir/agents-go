package agents

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// NewTool builds a Tool from a typed Go function. The argument
// type A (which must be a struct, or pointer to one) is reflected into a JSON
// Schema shown to the model; when the model calls the tool, the raw JSON
// arguments are validated against that schema, unmarshaled into A, and fn is
// invoked. The result R is returned to the model (serialized to JSON unless it
// is already a string).
//
// The schema is strict: every field is required and unknown properties are
// forbidden, and the OpenAI API then guarantees the model sends every field.
// Chain NonStrict to let the model omit fields whose json tag carries
// ",omitempty":
//
//	t := agents.NewTool("get_weather", "look up weather", weatherFn).NonStrict()
//
// NewTool panics when A cannot be reflected into a strict schema (not a
// struct, a field of an unsupported type, or a shape strict mode cannot express
// at all — an any/interface{} field, a map with arbitrary keys) — a
// deterministic programmer error, surfaced at construction like
// regexp.MustCompile. The strict schema is generated during construction, so
// chaining NonStrict cannot rescue that last case: build those tools with
// NewToolNonStrict. For a schema that is runtime data, use NewRawTool, which
// returns an error instead.
//
// The schema comes from reflection over A's struct tags, so the tool the model
// is shown and the Go type it decodes into cannot drift apart.
func NewTool[A any, R any](
	name, description string,
	fn func(ctx context.Context, tc *ToolContext, args A) (R, error),
) *Tool {
	return newTypedTool(name, description, true, fn)
}

// NewToolNonStrict is NewTool without the strict-mode rewrite: fields whose
// json tag carries ",omitempty" stay optional, and a shape strict mode cannot
// express — an any/interface{} field, a map with arbitrary keys — gets a schema
// instead of a panic. Everything else is identical, incoming arguments included:
// they are still validated against the schema the model was shown.
//
// It is the constructor for what NonStrict cannot reach. NonStrict relaxes a
// tool that already exists, and for these argument types NewTool never gets
// that far.
func NewToolNonStrict[A any, R any](
	name, description string,
	fn func(ctx context.Context, tc *ToolContext, args A) (R, error),
) *Tool {
	return newTypedTool(name, description, false, fn)
}

// newTypedTool is the shared body of NewTool and NewToolNonStrict; the two
// differ only in the strictness of the schema they generate.
func newTypedTool[A any, R any](
	name, description string,
	strict bool,
	fn func(ctx context.Context, tc *ToolContext, args A) (R, error),
) *Tool {
	// Strictness and constructor are one-to-one, so a panic can blame the call
	// the programmer actually wrote.
	ctor := "NewTool"
	if !strict {
		ctor = "NewToolNonStrict"
	}
	// Tool parameters must serialize to a JSON object, so A must be a struct
	// (or pointer to one).
	if argType := reflect.TypeFor[A](); !isStructKind(argType) {
		panic(fmt.Sprintf("agents: %s(%q): args type %s is not a struct (or pointer to struct); tool parameters must be a JSON object", ctor, name, argType))
	}
	regen := func(strict bool) (map[string]any, *schemaValidator) {
		schema, err := SchemaFor[A](strict)
		if err != nil {
			panic(fmt.Sprintf("agents: %s(%q): schema generation failed: %v", ctor, name, err))
		}
		// Compiled once per tool, not per call: a schema does not change
		// between turns.
		return schema, newSchemaValidator(schema)
	}
	t := &Tool{
		Name:                 name,
		Description:          description,
		Strict:               strict,
		FailureErrorFunction: DefaultToolErrorFunction,
		regen:                regen,
	}
	t.ParamsJSONSchema, t.validator = regen(strict)
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
// only argument shapes NewTool and NewToolNonStrict accept, since tool
// parameters must be a JSON object.
func isStructKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// toolArgumentsJSONError marks tool arguments that were not decodable JSON at
// all (a syntax error, as opposed to a shape/validation mismatch), so
// DefaultToolErrorFunction can use the dedicated "parsing tool arguments"
// wording for it. It unwraps to a *ModelBehaviorError like every other
// argument failure.
type toolArgumentsJSONError struct {
	mbe   *ModelBehaviorError
	cause error // the underlying JSON syntax error
}

func (e *toolArgumentsJSONError) Error() string { return e.mbe.Error() }
func (e *toolArgumentsJSONError) Unwrap() error { return e.mbe }

// decodeToolArgs decodes and validates the model-provided JSON argument string
// into dst. Every failure is a *ModelBehaviorError — fed back to the model
// via the tool's FailureErrorFunction so it can retry with corrected
// arguments:
// - undecodable JSON (syntax) — wrapped in toolArgumentsJSONError for the
// dedicated error wording,
// - a non-object payload,
// - anything the schema rejects, nested included,
// - a type mismatch while decoding into dst.
//
// An empty or whitespace-only string is treated as "{}" so tools taking a
// struct with all-optional fields still work.
func decodeToolArgs(toolName string, v *schemaValidator, argsJSON string, dst any) error {
	trimmed := strings.TrimSpace(argsJSON)
	trimmed = cmp.Or(trimmed, "{}")
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return &toolArgumentsJSONError{
			mbe:   NewModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err),
			cause: err,
		}
	}
	if _, ok := parsed.(map[string]any); !ok {
		return NewModelBehaviorError("Invalid JSON input for tool %s: expected a JSON object", toolName)
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
		return NewModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	if err := json.Unmarshal(filled, dst); err != nil {
		return NewModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	return nil
}

// NewRawTool builds a Tool from a pre-built JSON Schema map and
// a function that receives raw JSON arguments. Use this when the schema is
// loaded at runtime (e.g. from a database) rather than derived from a Go type —
// which is also why it returns an error where NewTool panics: a bad
// runtime schema is expected data, a bad argument type is a bug.
//
// Strict mode is enabled, and the schema is normalized to the strict subset via
// EnsureStrictJSONSchema, on a deep copy, so the caller's map is not mutated.
// Normalization rewrites what it accepts — every property becomes required,
// every object gets additionalProperties:false — so to advertise the schema
// verbatim instead, set ParamsJSONSchema and clear Strict on the returned tool.
//
// It runs before the tool exists, though, so a schema strict mode cannot express
// at all yields an error and no tool to fix up. Advertising one of those means
// building the Tool struct directly (Strict left false) and validating the
// arguments in its OnInvoke.
func NewRawTool(
	name, description string,
	paramsSchema map[string]any,
	fn func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error),
) (*Tool, error) {
	normalized, err := ensureStrictSchemaCopy(paramsSchema)
	if err != nil {
		return nil, fmt.Errorf("raw function tool %q: strict schema normalization failed: %w", name, err)
	}
	return &Tool{
		Name:                 name,
		Description:          description,
		ParamsJSONSchema:     normalized,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		OnInvoke:             fn,
	}, nil
}
