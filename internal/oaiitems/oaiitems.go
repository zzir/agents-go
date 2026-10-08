// Package oaiitems builds the Responses wire items openai-go has no one-call helper for.
package oaiitems

import (
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// FunctionCallOutput builds the function_call_output input item for callID.
func FunctionCallOutput(callID string, out responses.ResponseInputItemFunctionCallOutputOutputUnionParam) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
		CallID: param.NewOpt(callID),
		Output: out,
	}}
}
