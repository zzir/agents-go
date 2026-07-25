package agentstest

import (
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
)

// RawItem builds a model output item from its Responses-API wire JSON.
//
// It is the escape hatch behind the typed constructors below and the only way
// to script item shapes the SDK does not model — including types it does not
// recognize at all, which is exactly what a test for forward-compatible
// handling needs.
//
// The JSON is a literal in the test, not user input, so malformed input panics
// rather than returning an error: the panic points at the offending line.
func RawItem(rawJSON string) agents.TResponseOutputItem {
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(rawJSON), &item); err != nil {
		panic(fmt.Sprintf("agentstest: invalid output item JSON: %v\n%s", err, rawJSON))
	}
	return item
}

// MessageItem builds an assistant message carrying text.
func MessageItem(id, text string) agents.TResponseOutputItem {
	return RawItem(`{"type":"message","id":` + quote(id) +
		`,"status":"completed","role":"assistant","content":[{"type":"output_text","text":` +
		quote(text) + `,"annotations":[]}]}`)
}

// RefusalItem builds an assistant message carrying a refusal.
//
// A refusal takes precedence over any text in the same message, so a run
// scripted with one fails with an [agents.ModelRefusalError] unless a recovery
// handler is configured.
func RefusalItem(id, refusal string) agents.TResponseOutputItem {
	return RawItem(`{"type":"message","id":` + quote(id) +
		`,"status":"completed","role":"assistant","content":[{"type":"refusal","refusal":` +
		quote(refusal) + `}]}`)
}

// FunctionCallItem builds a function tool call. argsJSON is the raw arguments
// string the model would emit — pass invalid JSON to exercise the
// argument-parsing error path.
func FunctionCallItem(id, name, callID, argsJSON string) agents.TResponseOutputItem {
	return RawItem(`{"type":"function_call","id":` + quote(id) +
		`,"call_id":` + quote(callID) + `,"name":` + quote(name) +
		`,"arguments":` + quote(argsJSON) + `,"status":"completed"}`)
}

// ReasoningItem builds a reasoning item with a single summary part.
func ReasoningItem(id, text string) agents.TResponseOutputItem {
	return RawItem(`{"type":"reasoning","id":` + quote(id) +
		`,"summary":[{"type":"summary_text","text":` + quote(text) + `}]}`)
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // strings always marshal
		panic(err)
	}
	return string(b)
}
