package agentstest

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// The item constructors delegate to the modelkit builders; inputs are test
// literals, so a malformed one panics instead of returning an error.

// RawItem builds a model output item from its Responses-API wire JSON, the one
// way to script a shape the SDK does not model or recognize.
func RawItem(rawJSON string) agents.OutputItem {
	item, err := modelkit.OutputItemFromJSON([]byte(rawJSON))
	if err != nil {
		panic(fmt.Sprintf("agentstest: invalid output item JSON: %v\n%s", err, rawJSON))
	}
	return item
}

// MessageItem builds an assistant message carrying text.
func MessageItem(id, text string) agents.OutputItem {
	return must(modelkit.MessageItem(id, text))
}

// RefusalItem builds an assistant message carrying a refusal, which fails the
// run with an [agents.ModelRefusalError] unless a recovery handler is set.
func RefusalItem(id, refusal string) agents.OutputItem {
	return must(modelkit.RefusalItem(id, refusal))
}

// FunctionCallItem builds a function tool call; argsJSON is the raw arguments
// string (empty means "{}", invalid JSON exercises the parsing error path).
func FunctionCallItem(id, name, callID, argsJSON string) agents.OutputItem {
	return must(modelkit.FunctionCallItem(id, callID, name, argsJSON))
}

// reasoningSummaryPart is the wire shape of a native reasoning summary part.
type reasoningSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ReasoningItem builds a reasoning item with a single summary part, the native
// Responses shape; modelkit.ReasoningItem fills the content parts instead.
func ReasoningItem(id, text string) agents.OutputItem {
	raw, err := json.Marshal(struct {
		Type    string                 `json:"type"`
		ID      string                 `json:"id"`
		Summary []reasoningSummaryPart `json:"summary"`
	}{
		Type:    "reasoning",
		ID:      id,
		Summary: []reasoningSummaryPart{{Type: "summary_text", Text: text}},
	})
	if err != nil { // test literals always marshal
		panic(err)
	}
	return RawItem(string(raw))
}

func must(item agents.OutputItem, err error) agents.OutputItem {
	if err != nil {
		panic(fmt.Sprintf("agentstest: %v", err))
	}
	return item
}
