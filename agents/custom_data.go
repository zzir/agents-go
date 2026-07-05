package agents

import "encoding/json"

// FunctionToolCustomDataContext is passed to a FunctionTool's
// CustomDataExtractor after the tool ran and its output guardrails passed.
type FunctionToolCustomDataContext struct {
	// ToolContext is the invocation context of the call that produced Output.
	ToolContext *ToolContext
	// Tool is the function tool that was invoked.
	Tool *FunctionTool
	// Output is the model-visible tool output (the tool's return value, after
	// output guardrails).
	Output any
	// RawItem is the function_call_output input item that will be replayed to
	// the model. The extracted custom data is NOT part of it.
	RawItem TResponseInputItem
}

// normalizeCustomData enforces the JSON-compatible contract for SDK-only
// custom tool-output data: the value must survive a JSON round-trip (no
// NaN/Inf floats, channels, funcs, cycles, ...). It returns a decoupled deep
// copy so later mutations of the extractor's maps cannot leak into run items
// or serialized RunState. Nil and empty maps normalize to nil.
func normalizeCustomData(value map[string]any) (map[string]any, error) {
	if len(value) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, newUserError("CustomDataExtractor must return JSON-compatible data: %v", err)
	}
	var copied map[string]any
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, newUserError("CustomDataExtractor must return JSON-compatible data: %v", err)
	}
	return copied, nil
}
