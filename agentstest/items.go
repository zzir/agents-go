package agentstest

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// The item constructors delegate to the modelkit builders — one definition of
// "stamp raw wire JSON" for adapters and fakes alike — wrapped in the panic
// contract this package keeps: inputs are test literals, not user input, so a
// malformed one panics pointing at the offending line rather than threading an
// error through every test.

// RawItem builds a model output item from its Responses-API wire JSON.
//
// It is the escape hatch behind the typed constructors below and the only way
// to script item shapes the SDK does not model — including types it does not
// recognize at all, which is exactly what a test for forward-compatible
// handling needs.
func RawItem(rawJSON string) agents.TResponseOutputItem {
	item, err := modelkit.OutputItemFromJSON([]byte(rawJSON))
	if err != nil {
		panic(fmt.Sprintf("agentstest: invalid output item JSON: %v\n%s", err, rawJSON))
	}
	return item
}

// MessageItem builds an assistant message carrying text.
func MessageItem(id, text string) agents.TResponseOutputItem {
	return must(modelkit.MessageItem(id, text))
}

// RefusalItem builds an assistant message carrying a refusal.
//
// A refusal takes precedence over any text in the same message, so a run
// scripted with one fails with an [agents.ModelRefusalError] unless a recovery
// handler is configured.
func RefusalItem(id, refusal string) agents.TResponseOutputItem {
	return must(modelkit.RefusalItem(id, refusal))
}

// FunctionCallItem builds a function tool call. argsJSON is the raw arguments
// string the model would emit — pass invalid JSON to exercise the
// argument-parsing error path (empty means "{}").
func FunctionCallItem(id, name, callID, argsJSON string) agents.TResponseOutputItem {
	return must(modelkit.FunctionCallItem(id, callID, name, argsJSON))
}

// ReasoningItem builds a reasoning item with a single summary part.
//
// Deliberately NOT modelkit.ReasoningItem: that one places text in the content
// parts (raw reasoning, what a translated backend has), while this fake mimics
// the native Responses shape where the API returns a summary.
func ReasoningItem(id, text string) agents.TResponseOutputItem {
	return RawItem(`{"type":"reasoning","id":` + quote(id) +
		`,"summary":[{"type":"summary_text","text":` + quote(text) + `}]}`)
}

func must(item agents.TResponseOutputItem, err error) agents.TResponseOutputItem {
	if err != nil {
		panic(fmt.Sprintf("agentstest: %v", err))
	}
	return item
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // strings always marshal
		panic(err)
	}
	return string(b)
}
