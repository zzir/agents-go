package agents

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaValidator validates instances against one JSON Schema.
//
// The schema is compiled once and reused: resolving it per model call would
// redo the same work on every turn of every run, and a schema does not change
// between them.
type schemaValidator struct {
	once     sync.Once
	raw      map[string]any
	resolved *jsonschema.Resolved
	err      error
}

func newSchemaValidator(raw map[string]any) *schemaValidator {
	return &schemaValidator{raw: raw}
}

// resolve compiles the schema on first use.
//
// A schema this SDK cannot compile is NOT an error: it may still be a schema
// the provider understands, and refusing to run because we could not validate
// locally would turn a missing check into a broken feature. Validation is
// skipped instead.
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

// Validate reports whether raw satisfies the schema.
//
// It is the whole-schema check the hand-rolled one could not be: nested
// `required`, nested type mismatches, enums, bounds. The old check looked at
// root-level required keys only, so a model returning
// {"config": {"host": "x"}} against a schema requiring config.port produced a
// silent zero value instead of an error the model could recover from.
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
		// The library's messages carry a JSON-pointer path
		// ("/properties/inner/properties/port"), which is exactly what a model
		// needs to fix its own output.
		return err
	}
	return nil
}

// ApplyDefaults fills in the schema's default values for keys the instance
// omits, returning the completed JSON.
//
// A schema that documents a default and then hands the consumer a zero value is
// telling two different stories. Applying them here means a tool's argument
// struct sees what the schema advertised.
//
// It returns no error, and that is the contract rather than an oversight:
// defaults are a convenience, so anything that goes wrong — an uncompilable
// schema, a default that does not fit, a value that will not re-marshal —
// degrades to the input unchanged. Failing a call whose arguments are
// otherwise valid because a default could not be applied would be worse than
// not applying it. Validation still runs on whatever comes back.
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
// `additionalProperties: false` removed, for validation only.
//
// The schema SENT to the provider keeps it — that is what makes a model
// well-behaved, and OpenAI strict mode requires it. But enforcing it locally
// adds strictness without safety: an unexpected key is dropped by Go decoding
// and the tool cannot see it either way, so rejecting the call only turns a
// harmless extra into a failed turn. A misspelled key is still caught, by
// `required`, which is where it should be caught.
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
