package agents

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// handoffInputSchemas are the shapes validateHandoffInput is exercised against.
var (
	flatHandoffSchema = map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"reason": map[string]any{"type": "string"}},
		"required":             []any{"reason"},
		"additionalProperties": false,
	}
	nestedHandoffSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"config": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host": map[string]any{"type": "string"},
					"port": map[string]any{"type": "integer"},
				},
				"required": []any{"host", "port"},
			},
		},
		"required": []any{"config"},
	}
	enumHandoffSchema = map[string]any{
		"type":       "object",
		"properties": map[string]any{"tier": map[string]any{"type": "string", "enum": []any{"gold", "silver"}}},
		"required":   []any{"tier"},
	}
	// A hand-written schema need not spell out "type": "object", and JSON
	// Schema says nothing about `required` on a non-object instance.
	untypedHandoffSchema = map[string]any{
		"properties": map[string]any{"reason": map[string]any{"type": "string"}},
		"required":   []any{"reason"},
	}
	// Resolving fails on the malformed pattern, so only the checks that need no
	// compiled schema survive.
	uncompilableHandoffSchema = map[string]any{
		"type":       "object",
		"properties": map[string]any{"reason": map[string]any{"type": "string", "pattern": "([a-z"}},
		"required":   []any{"reason"},
	}
)

// A handoff input schema is enforced whole: the old check looked at root-level
// required keys only, so a nested miss, a wrong type or a violated enum reached
// the target agent as a zero value it had no way to notice.
func TestValidateHandoffInput(t *testing.T) {
	tests := []struct {
		name    string
		schema  map[string]any
		args    string
		wantErr bool
	}{
		{name: "valid flat input", schema: flatHandoffSchema, args: `{"reason":"needs billing"}`},
		{name: "root type mismatch", schema: flatHandoffSchema, args: `{"reason":123}`, wantErr: true},
		{name: "root required key missing", schema: flatHandoffSchema, args: `{"other":"x"}`, wantErr: true},
		{name: "malformed JSON", schema: flatHandoffSchema, args: `{"reason":`, wantErr: true},

		{name: "valid nested input", schema: nestedHandoffSchema, args: `{"config":{"host":"h","port":80}}`},
		{name: "nested required key missing", schema: nestedHandoffSchema, args: `{"config":{"host":"h"}}`, wantErr: true},
		{name: "nested type mismatch", schema: nestedHandoffSchema, args: `{"config":{"host":"h","port":"80"}}`, wantErr: true},

		{name: "enum satisfied", schema: enumHandoffSchema, args: `{"tier":"gold"}`},
		{name: "enum violated", schema: enumHandoffSchema, args: `{"tier":"bronze"}`, wantErr: true},

		// Handoff input is an object or it is nothing. `required` alone says
		// nothing about a scalar, so without this the schema below would accept
		// one and the target agent would be handed input it cannot read.
		{name: "untyped schema, valid object", schema: untypedHandoffSchema, args: `{"reason":"x"}`},
		{name: "untyped schema, scalar payload", schema: untypedHandoffSchema, args: `5`, wantErr: true},
		{name: "untyped schema, array payload", schema: untypedHandoffSchema, args: `["reason"]`, wantErr: true},

		// A transfer that takes no input accepts every spelling of "nothing to
		// send", plus the extra keys strict mode forbids remotely but local
		// validation deliberately tolerates. It is still a transfer whose
		// schema says "object", so a payload that is not one is rejected like
		// any other tool's would be.
		{name: "no-input transfer, empty args", schema: emptyStrictSchema(), args: ``},
		{name: "no-input transfer, null args", schema: emptyStrictSchema(), args: `null`},
		{name: "no-input transfer, empty object", schema: emptyStrictSchema(), args: `{}`},
		{name: "no-input transfer, extra key", schema: emptyStrictSchema(), args: `{"reason":"x"}`},
		{name: "no-input transfer, array args", schema: emptyStrictSchema(), args: `[]`, wantErr: true},
		{name: "no-input transfer, unparseable args", schema: emptyStrictSchema(), args: `None`, wantErr: true},

		// A schema that will not compile drops to the checks that need no
		// compiled schema rather than to no checks at all.
		{name: "uncompilable schema, absent input", schema: uncompilableHandoffSchema, args: ``, wantErr: true},
		{name: "uncompilable schema, scalar payload", schema: uncompilableHandoffSchema, args: `5`, wantErr: true},
		{name: "uncompilable schema, wrong key", schema: uncompilableHandoffSchema, args: `{"other":1}`},

		// No schema means nothing to check against, not a stricter default.
		{name: "no schema", schema: nil, args: `not json at all`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Handoff{ToolName: "transfer_to_billing", InputJSONSchema: tt.schema}
			err := validateHandoffInput(&h, tt.args)
			if tt.wantErr && err == nil {
				t.Fatalf("args %s were accepted, want a *ModelBehaviorError", tt.args)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("args %s were rejected: %v", tt.args, err)
				}
				return
			}
			var mbe *ModelBehaviorError
			if !errors.As(err, &mbe) {
				t.Fatalf("err = %v (%T), want *ModelBehaviorError", err, err)
			}
		})
	}
}

// A handoff that expects input and receives none keeps the wording the model
// (and docs/handoffs.md) have always seen.
func TestValidateHandoffInput_AbsentInputWording(t *testing.T) {
	h := Handoff{ToolName: "transfer_to_billing", InputJSONSchema: flatHandoffSchema}
	for _, args := range []string{"", "  ", "null"} {
		err := validateHandoffInput(&h, args)
		if err == nil {
			t.Fatalf("args %q were accepted, want a *ModelBehaviorError", args)
		}
		if !strings.Contains(err.Error(), "expected non-null input") {
			t.Errorf("args %q: message = %q, want the non-null-input text", args, err.Error())
		}
	}
}

// The rejection names the offending path, which is what a model needs to
// correct itself.
func TestValidateHandoffInput_ErrorNamesThePath(t *testing.T) {
	h := Handoff{ToolName: "transfer_to_billing", InputJSONSchema: nestedHandoffSchema}
	err := validateHandoffInput(&h, `{"config":{"host":"h"}}`)
	if err == nil {
		t.Fatal("a nested missing key was accepted")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("err = %v, want it to name the missing nested field", err)
	}
	if !strings.Contains(err.Error(), "transfer_to_billing") {
		t.Errorf("err = %v, want it to name the handoff", err)
	}
}

// Validation leaves the Handoff alone. Users copy and share Handoff values
// freely, so caching the compiled schema on one would be a data race the
// runner's per-turn copies only happen to hide.
func TestValidateHandoffInput_Concurrent(t *testing.T) {
	h := &Handoff{ToolName: "transfer_to_billing", InputJSONSchema: flatHandoffSchema}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := validateHandoffInput(h, `{"reason":"needs billing"}`); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
