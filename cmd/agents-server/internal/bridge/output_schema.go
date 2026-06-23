package bridge

import (
	"encoding/json"

	"github.com/zzir/agents-go/agents"
)

// BuildOutputSchema builds a dynamic output schema from a JSON Schema string, or nil if empty/invalid.
func BuildOutputSchema(schemaJSON string) agents.OutputSchema {
	if schemaJSON == "" {
		return nil
	}
	var schema map[string]any
	if json.Unmarshal([]byte(schemaJSON), &schema) != nil {
		return nil
	}
	return agents.NewDynamicOutputSchema("final_output", schema, true)
}
