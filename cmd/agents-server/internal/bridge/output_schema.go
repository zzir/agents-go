package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents"
)

// BuildOutputSchema builds a dynamic output schema from a JSON Schema string.
// An empty string yields (nil, nil) — no structured output. A malformed
// schema, or one strict mode cannot express, is a config error returned to the
// caller, never a constraint quietly dropped or left for the first run to hit.
func BuildOutputSchema(schemaJSON string) (agents.OutputSchema, error) {
	if schemaJSON == "" {
		return nil, nil
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return nil, fmt.Errorf("output_schema is not valid JSON: %w", err)
	}
	if err := CheckStrictOutputSchema(schema); err != nil {
		return nil, err
	}
	return agents.NewDynamicOutputSchema("final_output", schema, true)
}

// CheckStrictOutputSchema reports why schema cannot be sent in strict mode, or
// nil. It checks a copy, since the SDK's check rewrites what it is given.
func CheckStrictOutputSchema(schema map[string]any) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("output_schema is not valid JSON: %w", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("output_schema is not valid JSON: %w", err)
	}
	if _, err := agents.EnsureStrictJSONSchema(probe); err != nil {
		// What follows the semicolon advises a Go caller to turn strict off; the
		// workbench has no such switch.
		reason, _, _ := strings.Cut(err.Error(), "; ")
		return fmt.Errorf("output_schema cannot be used in strict mode: %s", reason)
	}
	return nil
}
