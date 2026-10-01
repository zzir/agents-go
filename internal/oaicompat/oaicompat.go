// Package oaicompat compiles against either shape of CallID — see decisions §5.5b.
package oaicompat

import (
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// FunctionCallOutput builds the function_call_output input item for callID.
func FunctionCallOutput(callID string, out responses.ResponseInputItemFunctionCallOutputOutputUnionParam) responses.ResponseInputItemUnionParam {
	p := responses.ResponseInputItemFunctionCallOutputParam{Output: out}
	switch id := any(&p.CallID).(type) {
	case *string:
		*id = callID
	case *param.Opt[string]:
		*id = param.NewOpt(callID)
	}
	return responses.ResponseInputItemUnionParam{OfFunctionCallOutput: &p}
}

// CallID reads the call id off a function_call_output item.
func CallID(p *responses.ResponseInputItemFunctionCallOutputParam) string {
	switch id := any(p.CallID).(type) {
	case string:
		return id
	case param.Opt[string]:
		return id.Or("")
	}
	return ""
}
