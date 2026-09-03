package agents

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// NewTool builds a Tool from a typed Go function. The argument type A (a
// struct, or pointer to one) is reflected into a strict JSON Schema shown to
// the model: every field required, unknown properties forbidden. Arguments
// are validated against it (spec §2.7h), decoded into A, and fn is invoked;
// R goes back to the model (JSON unless already a string). Chain NonStrict to
// let fields tagged ",omitempty" be omitted. NewTool panics when A cannot be
// reflected into a strict schema (decisions §5.11); an any field or open map
// needs NewToolNonStrict, and a runtime schema NewRawTool, which errors instead.
func NewTool[A any, R any](
	name, description string,
	fn func(ctx context.Context, tc *ToolContext, args A) (R, error),
) *Tool {
	return newTypedTool(name, description, true, fn)
}

// NewToolNonStrict is NewTool without the strict-mode rewrite: ",omitempty"
// fields stay optional, and a shape strict mode cannot express gets a schema
// instead of a panic. Arguments are still validated against the schema.
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
	// Name the constructor in panics so they blame the call the programmer wrote.
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

// isStructKind reports whether t is a struct or a pointer to one — the only
// argument shapes a tool accepts, since parameters must be a JSON object.
func isStructKind(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// toolArgumentsJSONError marks arguments that were not decodable JSON at all,
// for DefaultToolErrorFunction's wording; unwraps to a *ModelBehaviorError.
type toolArgumentsJSONError struct {
	mbe   *ModelBehaviorError
	cause error // the underlying JSON syntax error
}

func (e *toolArgumentsJSONError) Error() string { return e.mbe.Error() }
func (e *toolArgumentsJSONError) Unwrap() error { return e.mbe }

// decodeToolArgs validates and decodes the model's JSON arguments into dst;
// every failure is a *ModelBehaviorError (spec §2.7h). Empty reads as "{}".
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
	// Fill in schema defaults before validating, so a documented default reaches
	// the tool rather than a zero value.
	filled := v.ApplyDefaults([]byte(trimmed))
	// Validate the whole schema, nested included: a nested missing required field
	// or type mismatch must not reach the tool as an unnoticed zero value.
	if err := v.Validate(filled); err != nil {
		return NewModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	if err := json.Unmarshal(filled, dst); err != nil {
		return NewModelBehaviorError("Invalid JSON input for tool %s: %v", toolName, err)
	}
	return nil
}

// NewRawTool builds a Tool from a pre-built JSON Schema map and a function
// taking raw JSON arguments — for a schema that is runtime data, which is why
// it returns an error where NewTool panics (decisions §5.11). Strict mode is
// on: the schema is normalized via EnsureStrictJSONSchema on a deep copy. To
// advertise a schema verbatim, set ParamsJSONSchema and clear Strict on the
// result; a schema strict mode cannot express at all needs a hand-built Tool.
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
