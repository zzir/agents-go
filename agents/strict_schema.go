package agents

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// The bounds on one conversion: a node is strictened once however many $refs
// reach it, so a legitimate schema never nears either.
const (
	strictMaxNodes   = 1 << 16
	strictMaxRefHops = 64
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

// EnsureStrictJSONSchema rewrites a JSON Schema map in place into the strict
// subset the OpenAI API expects: additionalProperties:false, every property
// required, oneOf folded into anyOf, single allOf inlined, null defaults
// stripped, $refs with siblings unraveled.
func EnsureStrictJSONSchema(schema map[string]any) (map[string]any, error) {
	if len(schema) == 0 {
		return emptyStrictSchema(), nil
	}
	w := &strictWalk{root: schema, seen: make(map[uintptr]bool)}
	if err := w.ensureStrict(schema, nil); err != nil {
		return nil, err
	}
	return schema, nil
}

// strictWalk is one conversion: the root the $refs resolve against and the
// maps already made strict (an unraveled $ref shares its target's maps).
type strictWalk struct {
	root map[string]any
	seen map[uintptr]bool
}

// ensureStrictSchemaCopy runs EnsureStrictJSONSchema on a JSON round-trip copy of
// schema, leaving the caller's map unmutated.
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

// errUnconstrainedSchema builds the construction-time error for a node that
// constrains no value: a boolean schema or a typeless object from an any field.
func errUnconstrainedSchema(what string, path []string) error {
	return fmt.Errorf(
		"%s (path=%s) is an unconstrained schema: a Go any/interface{} field has no concrete "+
			"type (it reflects to a boolean schema, or to a typeless object when it carries a "+
			"description tag) and cannot be expressed in strict mode; give the field a concrete "+
			"type (a struct or a specific scalar/slice/map), or turn strict off where this "+
			"schema was built — for a Go type that means NewToolNonStrict / OutputTypeNonStrict, "+
			"not Tool.NonStrict, which runs after the schema is generated",
		what, strings.Join(path, "/"))
}

// isUnconstrainedSchema reports whether a node declares no "type" and no stand-in
// for one ($ref, combinator, enum/const, object/array structure) — an any field.
func isUnconstrainedSchema(node map[string]any) bool {
	for _, k := range []string{"type", "$ref", "anyOf", "oneOf", "allOf", "enum", "const", "properties", "items"} {
		if _, ok := node[k]; ok {
			return false
		}
	}
	return true
}

func (w *strictWalk) ensureStrict(node map[string]any, path []string) error {
	id := reflect.ValueOf(node).Pointer()
	if w.seen[id] {
		return nil
	}
	if len(w.seen) >= strictMaxNodes {
		return fmt.Errorf("schema has more than %d nodes (path=%s)", strictMaxNodes, strings.Join(path, "/"))
	}
	w.seen[id] = true
	hops := 0
	for {
		unraveled, err := w.strictenOnce(node, path)
		if err != nil {
			return err
		}
		if !unraveled {
			return nil
		}
		// The merged-in keys need the same pass; a $ref chain that keeps
		// producing a $ref is cut rather than followed forever.
		if hops++; hops > strictMaxRefHops {
			return fmt.Errorf("$ref chain longer than %d at path=%s", strictMaxRefHops, strings.Join(path, "/"))
		}
	}
}

// strictenOnce makes node strict and reports whether it unraveled a $ref,
// which leaves node with keys this pass has not seen.
func (w *strictWalk) strictenOnce(node map[string]any, path []string) (bool, error) {
	// Recurse into $defs and definitions.
	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, ok := node[defsKey].(map[string]any); ok {
			for name, def := range defs {
				ds, ok := def.(map[string]any)
				if !ok {
					continue
				}
				if err := w.ensureStrict(ds, append(path, defsKey, name)); err != nil {
					return false, err
				}
			}
		}
	}

	typ, _ := node["type"].(string)
	if typ == "object" {
		if _, has := node["additionalProperties"]; !has {
			node["additionalProperties"] = false
		} else if isTruthy(node["additionalProperties"]) {
			return false, fmt.Errorf(
				"additionalProperties should not be set to true for object types in a strict schema "+
					"(path=%s); if you need open objects, turn strict off where this schema was "+
					"built — for a Go type that means NewToolNonStrict / OutputTypeNonStrict",
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
					return false, errUnconstrainedSchema(fmt.Sprintf("property %q", key), append(path, "properties", key))
				}
				continue
			}
			if err := w.ensureStrict(ps, append(path, "properties", key)); err != nil {
				return false, err
			}
			// A property that still constrains nothing after normalization (the map
			// form of an any/interface{} field) would 400 at request time; reject it.
			if isUnconstrainedSchema(ps) {
				return false, errUnconstrainedSchema(fmt.Sprintf("property %q", key), append(path, "properties", key))
			}
		}
	}

	// Array items.
	if items, ok := node["items"].(map[string]any); ok {
		if err := w.ensureStrict(items, append(path, "items")); err != nil {
			return false, err
		}
		if isUnconstrainedSchema(items) {
			return false, errUnconstrainedSchema("array items", append(path, "items"))
		}
	} else if _, isBool := node["items"].(bool); isBool {
		return false, errUnconstrainedSchema("array items", append(path, "items"))
	}

	// Unions.
	if anyOf, ok := node["anyOf"].([]any); ok {
		for i, variant := range anyOf {
			vs, ok := variant.(map[string]any)
			if !ok {
				continue
			}
			if err := w.ensureStrict(vs, append(path, "anyOf", strconv.Itoa(i))); err != nil {
				return false, err
			}
		}
	}

	// oneOf -> anyOf (OpenAI structured outputs reject nested oneOf).
	if oneOf, ok := node["oneOf"].([]any); ok {
		existing, _ := node["anyOf"].([]any)
		for i, variant := range oneOf {
			if vs, ok := variant.(map[string]any); ok {
				if err := w.ensureStrict(vs, append(path, "oneOf", strconv.Itoa(i))); err != nil {
					return false, err
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
				if err := w.ensureStrict(only, append(path, "allOf", "0")); err != nil {
					return false, err
				}
				maps.Copy(node, only)
			}
			delete(node, "allOf")
		} else {
			for i, entry := range allOf {
				if es, ok := entry.(map[string]any); ok {
					if err := w.ensureStrict(es, append(path, "allOf", strconv.Itoa(i))); err != nil {
						return false, err
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
		resolved, err := resolveRef(w.root, ref)
		if err != nil {
			return false, err
		}
		delete(node, "$ref")
		// Node's own keys take priority over the resolved schema's.
		for k, v := range resolved {
			if _, exists := node[k]; !exists {
				node[k] = v
			}
		}
		return true, nil
	}

	return false, nil
}

// sortAnyStrings sorts a slice of any whose elements are strings, in place, keeping
// a generated "required" list aligned with json.Marshal's sorted property keys.
func sortAnyStrings(s []any) {
	slices.SortFunc(s, func(a, b any) int {
		as, _ := a.(string)
		bs, _ := b.(string)
		return cmp.Compare(as, bs)
	})
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
