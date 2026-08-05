package agents

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// emptyStrictSchema is the canonical empty object schema OpenAI strict mode
// expects for a tool that takes no arguments.
func emptyStrictSchema() map[string]any {
	return map[string]any{
		"additionalProperties": false,
		"type":                 "object",
		"properties":           map[string]any{},
		"required":             []any{},
	}
}

// EnsureStrictJSONSchema rewrites a JSON Schema (as a map) in place so it
// conforms to the strict subset the OpenAI API expects: every object gets
// additionalProperties:false, every property becomes required, oneOf is folded
// into anyOf, single-element allOf is inlined, null defaults are stripped, and
// $refs carrying sibling keys are unraveled.
//
// It is the compatibility-critical path: getting it wrong yields 400s from the
// API, on requests that look fine locally.
func EnsureStrictJSONSchema(schema map[string]any) (map[string]any, error) {
	if len(schema) == 0 {
		return emptyStrictSchema(), nil
	}
	if err := ensureStrict(schema, nil, schema); err != nil {
		return nil, err
	}
	return schema, nil
}

// ensureStrictSchemaCopy deep-copies schema (via a JSON round-trip) and runs
// EnsureStrictJSONSchema on the copy, so callers holding a user-supplied map
// get a normalized schema without mutating the caller's value.
func ensureStrictSchemaCopy(schema map[string]any) (map[string]any, error) {
	if len(schema) == 0 {
		return emptyStrictSchema(), nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("schema is not JSON-marshalable: %w", err)
	}
	var copied map[string]any
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, fmt.Errorf("copying schema: %w", err)
	}
	return EnsureStrictJSONSchema(copied)
}

// errUnconstrainedSchema builds the error for a JSON Schema node that constrains
// no value. This has two forms, both produced by a Go any/interface{} field: a
// boolean "true"/"false" schema (an untagged field), and a bare typeless object
// such as {"description":...} (a field carrying a jsonschema description tag).
// Strict mode has no way to express an unconstrained value, so surface the
// problem at construction time instead of a 400 from the API at request time.
//
// json.RawMessage / []byte are deliberately not suggested as a fix: they reflect
// to a byte-array schema (a JSON array of integers 0-255), not an arbitrary-JSON
// schema, so they change the argument's meaning rather than relaxing it. Give
// the field a concrete type, or disable strict mode for this tool/output.
func errUnconstrainedSchema(what string, path []string) error {
	return fmt.Errorf(
		"%s (path=%s) is an unconstrained schema: a Go any/interface{} field has no concrete "+
			"type (it reflects to a boolean schema, or to a typeless object when it carries a "+
			"description tag) and cannot be expressed in strict mode; give the field a concrete "+
			"type (a struct or a specific scalar/slice/map) or disable strict mode for this tool/output",
		what, strings.Join(path, "/"))
}

// isUnconstrainedSchema reports whether a normalized schema node constrains no
// value: it declares neither a concrete "type" nor any construct that stands in
// for one (a $ref, a combinator, an enum/const, or object/array structure).
// Such a node is the map form of an any/interface{} field — e.g.
// {"description":...} — and, like a boolean "true" schema, is rejected by
// strict mode.
func isUnconstrainedSchema(node map[string]any) bool {
	for _, k := range []string{"type", "$ref", "anyOf", "oneOf", "allOf", "enum", "const", "properties", "items"} {
		if _, ok := node[k]; ok {
			return false
		}
	}
	return true
}

func ensureStrict(node map[string]any, path []string, root map[string]any) error {
	// Recurse into $defs and definitions.
	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, ok := node[defsKey].(map[string]any); ok {
			for name, def := range defs {
				ds, ok := def.(map[string]any)
				if !ok {
					continue
				}
				if err := ensureStrict(ds, append(path, defsKey, name), root); err != nil {
					return err
				}
			}
		}
	}

	typ, _ := node["type"].(string)
	if typ == "object" {
		if _, has := node["additionalProperties"]; !has {
			node["additionalProperties"] = false
		} else if isTruthy(node["additionalProperties"]) {
			return fmt.Errorf(
				"additionalProperties should not be set to true for object types in a strict schema "+
					"(path=%s); disable strict mode for this tool/output if you need open objects",
				strings.Join(path, "/"))
		}
		// OpenAI requires every object to declare "properties"; schemas for
		// empty structs (and some MCP servers) omit it.
		if _, has := node["properties"]; !has {
			node["properties"] = map[string]any{}
		}
	}

	// Object properties: mark all required and recurse.
	if props, ok := node["properties"].(map[string]any); ok {
		required := make([]any, 0, len(props))
		for key := range props {
			required = append(required, key)
		}
		// Preserve a deterministic order for reproducible output.
		sortAnyStrings(required)
		node["required"] = required
		for key, prop := range props {
			ps, ok := prop.(map[string]any)
			if !ok {
				if _, isBool := prop.(bool); isBool {
					return errUnconstrainedSchema(fmt.Sprintf("property %q", key), append(path, "properties", key))
				}
				continue
			}
			if err := ensureStrict(ps, append(path, "properties", key), root); err != nil {
				return err
			}
			// A property that still constrains nothing after normalization — the
			// map form of an any/interface{} field, e.g. {"description":...} —
			// slips past the boolean-schema guard above but would 400 at request
			// time, so reject it here too.
			if isUnconstrainedSchema(ps) {
				return errUnconstrainedSchema(fmt.Sprintf("property %q", key), append(path, "properties", key))
			}
		}
	}

	// Array items.
	if items, ok := node["items"].(map[string]any); ok {
		if err := ensureStrict(items, append(path, "items"), root); err != nil {
			return err
		}
		if isUnconstrainedSchema(items) {
			return errUnconstrainedSchema("array items", append(path, "items"))
		}
	} else if _, isBool := node["items"].(bool); isBool {
		return errUnconstrainedSchema("array items", append(path, "items"))
	}

	// Unions.
	if anyOf, ok := node["anyOf"].([]any); ok {
		for i, variant := range anyOf {
			vs, ok := variant.(map[string]any)
			if !ok {
				continue
			}
			if err := ensureStrict(vs, append(path, "anyOf", strconv.Itoa(i)), root); err != nil {
				return err
			}
		}
	}

	// oneOf -> anyOf (OpenAI structured outputs reject nested oneOf).
	if oneOf, ok := node["oneOf"].([]any); ok {
		existing, _ := node["anyOf"].([]any)
		for i, variant := range oneOf {
			if vs, ok := variant.(map[string]any); ok {
				if err := ensureStrict(vs, append(path, "oneOf", strconv.Itoa(i)), root); err != nil {
					return err
				}
			}
			existing = append(existing, variant)
		}
		node["anyOf"] = existing
		delete(node, "oneOf")
	}

	// Intersections.
	if allOf, ok := node["allOf"].([]any); ok {
		if len(allOf) == 1 {
			if only, ok := allOf[0].(map[string]any); ok {
				if err := ensureStrict(only, append(path, "allOf", "0"), root); err != nil {
					return err
				}
				maps.Copy(node, only)
			}
			delete(node, "allOf")
		} else {
			for i, entry := range allOf {
				if es, ok := entry.(map[string]any); ok {
					if err := ensureStrict(es, append(path, "allOf", strconv.Itoa(i)), root); err != nil {
						return err
					}
				}
			}
		}
	}

	// Strip null defaults.
	if def, has := node["default"]; has && def == nil {
		delete(node, "default")
	}

	// Unravel a $ref that carries sibling keys.
	if ref, ok := node["$ref"].(string); ok && len(node) > 1 {
		resolved, err := resolveRef(root, ref)
		if err != nil {
			return err
		}
		delete(node, "$ref")
		// Node's own keys take priority over the resolved schema's.
		for k, v := range resolved {
			if _, exists := node[k]; !exists {
				node[k] = v
			}
		}
		return ensureStrict(node, path, root)
	}

	return nil
}

func resolveRef(root map[string]any, ref string) (map[string]any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("unexpected $ref format %q: does not start with #/", ref)
	}
	cur := root
	for key := range strings.SplitSeq(ref[2:], "/") {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("non-object entry while resolving $ref %q at %q", ref, key)
		}
		cur = next
	}
	return cur, nil
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case nil:
		return false
	default:
		return true
	}
}
