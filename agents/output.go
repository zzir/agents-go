package agents

// OutputSchema describes the structured output a model is asked to produce. It
// is the Go counterpart of the Python SDK's AgentOutputSchemaBase abstract base
// class. The OpenAI model adapter uses it to build the response_format payload
// and to validate/parse the model's final output.
//
// The full reflection-based implementation lands in Phase 2; this interface is
// defined here because the Model interface depends on it.
type OutputSchema interface {
	// IsPlainText reports whether the output is unstructured text (no schema).
	IsPlainText() bool
	// Name is the schema name sent to the provider (e.g. "final_output").
	Name() string
	// JSONSchema returns the JSON Schema for the output object. It is only
	// called when IsPlainText reports false.
	JSONSchema() map[string]any
	// IsStrictJSONSchema reports whether strict-mode validation is requested.
	IsStrictJSONSchema() bool
	// ValidateJSON parses and validates a raw JSON string produced by the model,
	// returning the decoded Go value.
	ValidateJSON(jsonStr string) (any, error)
}
