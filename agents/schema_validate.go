package agents

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaValidator validates instances against one JSON Schema, compiled once
// and reused across calls.
type schemaValidator struct {
	once     sync.Once
	raw      map[string]any
	resolved *jsonschema.Resolved
	err      error
}

func newSchemaValidator(raw map[string]any) *schemaValidator {
	return &schemaValidator{raw: raw}
}

// resolve compiles the schema on first use. A schema this SDK cannot compile is
// not an error — validation is skipped rather than failing the run.
func (v *schemaValidator) resolve() (*jsonschema.Resolved, bool) {
	v.once.Do(func() {
		if len(v.raw) == 0 {
			return
		}
		b, err := json.Marshal(relaxAdditionalProperties(v.raw))
		if err != nil {
			v.err = err
			return
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(b, &s); err != nil {
			v.err = err
			return
		}
		v.resolved, v.err = s.Resolve(nil)
	})
	return v.resolved, v.resolved != nil && v.err == nil
}

// Validate reports whether raw satisfies the whole schema, nested included
// (nested required, type mismatches, enums, bounds).
func (v *schemaValidator) Validate(raw []byte) error {
	res, ok := v.resolve()
	if !ok {
		return nil
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := res.Validate(instance); err != nil {
		// The library's messages carry a JSON-pointer path, which is what a model
		// needs to fix its own output.
		return err
	}
	return nil
}

// ApplyDefaults fills in the schema's default values for keys the instance
// omits, returning the completed JSON. It never errors: anything that goes
// wrong (uncompilable schema, unfittable default, re-marshal failure) degrades
// to the input unchanged.
func (v *schemaValidator) ApplyDefaults(raw []byte) []byte {
	res, ok := v.resolve()
	if !ok {
		return raw
	}
	var instance any
	if json.Unmarshal(raw, &instance) != nil {
		return raw
	}
	if res.ApplyDefaults(&instance) != nil {
		return raw
	}
	out, err := json.Marshal(instance)
	if err != nil {
		return raw
	}
	return out
}

// relaxAdditionalProperties returns a copy of schema with every
// `additionalProperties: false` removed, for local validation only — the schema
// sent to the provider keeps it. Enforcing it locally would only turn a
// harmless extra key into a failed turn; a misspelled key is still caught by
// `required`.
func relaxAdditionalProperties(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, val := range schema {
		if k == "additionalProperties" {
			if b, ok := val.(bool); ok && !b {
				continue
			}
		}
		switch v := val.(type) {
		case map[string]any:
			out[k] = relaxAdditionalProperties(v)
		case []any:
			cp := make([]any, len(v))
			for i, e := range v {
				if m, ok := e.(map[string]any); ok {
					cp[i] = relaxAdditionalProperties(m)
				} else {
					cp[i] = e
				}
			}
			out[k] = cp
		default:
			out[k] = val
		}
	}
	return out
}
