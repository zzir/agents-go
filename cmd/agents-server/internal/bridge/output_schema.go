package bridge

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// BuildOutputSchema builds a dynamic output schema from a JSON Schema string.
// An empty string yields (nil, nil) — no structured output. A non-empty but
// malformed schema is a config error, NOT silently ignored: returning it lets
// the caller fail the run (or reject the config) instead of quietly dropping
// the requested structured-output constraint.
func BuildOutputSchema(schemaJSON string) (agents.OutputSchema, error) {
	if schemaJSON == "" {
		return nil, nil
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return nil, fmt.Errorf("output_schema is not valid JSON: %w", err)
	}
	return agents.NewDynamicOutputSchema("final_output", schema, true), nil
}
