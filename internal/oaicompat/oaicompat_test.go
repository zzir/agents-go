package oaicompat

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// The item carries its call id on the wire and gives it back, whichever type
// openai-go declares the field with.
func TestFunctionCallOutputCarriesTheCallID(t *testing.T) {
	item := FunctionCallOutput("call_1", responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt("sunny")})
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Type != "function_call_output" || wire.CallID != "call_1" || wire.Output != "sunny" {
		t.Fatalf("wire item = %s", raw)
	}
	if got := CallID(item.OfFunctionCallOutput); got != "call_1" {
		t.Fatalf("CallID() = %q, want call_1", got)
	}
}
