package agents

import (
	"context"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/tracing"
)

// usageFromStreamResponse extracts token usage from a streamed final Response.
// When the response carries no usage block it counts as zero requests (matching
// the blocking path's usageFromResponse and Python's `Usage()` fallback), so a
// backend that omits usage does not inflate the request count.
func usageFromStreamResponse(resp *responses.Response) *Usage {
	if !resp.JSON.Usage.Valid() {
		return NewUsage()
	}
	u := resp.Usage
	return &Usage{
		Requests:     1,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
		InputTokensDetails: InputTokensDetails{
			CachedTokens:     u.InputTokensDetails.CachedTokens,
			CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
		},
		OutputTokensDetails: OutputTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens},
	}
}

// streamOneModelCall streams a single model call, forwarding each raw event to
// the consumer and assembling the final ModelResponse from the completed event.
// Only Run takes this path; RunSync makes one blocking GetResponse call.
// The first event's arrival is stamped on the generation span as
// time_to_first_token_ms.
func (r *runner) streamOneModelCall(ctx context.Context, span *tracing.SpanHandle, model Model, req ModelRequest) (*ModelResponse, error) {
	asm := &responseAssembler{}
	start := time.Now()
	first := false
	// The stamp waits for the first DELTA — the first actual token. Earlier
	// events carry none: response.created arrives immediately (it would
	// measure connection setup) and response.output_item.added only announces
	// an item whose content is still to come. Terminal events stamp as a
	// fallback so a stream that carries only its final payload still records
	// something.
	stamp := func() {
		if !first {
			first = true
			span.Set("time_to_first_token_ms", time.Since(start).Milliseconds())
		}
	}
	for event, err := range model.StreamResponse(ctx, req) {
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		if strings.HasSuffix(event.Type, ".delta") ||
			event.Type == "response.completed" || event.Type == "response.incomplete" {
			stamp()
		}
		if !r.emit(&RawResponsesStreamEvent{Data: event}) {
			return nil, errConsumerStopped
		}
		asm.observe(event)
	}
	return asm.result()
}

// responseAssembler assembles the final ModelResponse from a raw Responses
// event stream. It is the one place stream events become a ModelResponse —
// the runner's streaming path and the stream-only adapter (NewStreamOnlyModel)
// both feed it, so the two paths cannot drift.
type responseAssembler struct {
	final *ModelResponse
	items []TResponseOutputItem
}

func (a *responseAssembler) observe(event *TResponseStreamEvent) {
	switch event.Type {
	case "response.output_item.done":
		// Collected as a fallback for backends (e.g. ChatGPT with store=false)
		// whose terminal event carries an empty Output array.
		done := event.AsResponseOutputItemDone()
		a.items = append(a.items, done.Item)
	case "response.completed":
		completed := event.AsResponseCompleted()
		a.final = &ModelResponse{
			Output:     completed.Response.Output,
			Usage:      usageFromStreamResponse(&completed.Response),
			ResponseID: completed.Response.ID,
			Status:     string(completed.Response.Status),
		}
	case "response.incomplete":
		// A response cut off at the output-token limit still arrived. It is
		// assembled like any other so the runner can see it is truncated
		// and refuse to run its tool calls; treating it as "no response"
		// would throw the turn away over a length limit.
		inc := event.AsResponseIncomplete()
		a.final = &ModelResponse{
			Output:           inc.Response.Output,
			Usage:            usageFromStreamResponse(&inc.Response),
			ResponseID:       inc.Response.ID,
			Status:           string(inc.Response.Status),
			IncompleteReason: inc.Response.IncompleteDetails.Reason,
		}
	}
}

func (a *responseAssembler) result() (*ModelResponse, error) {
	if a.final == nil {
		// No response.completed event arrived: the stream ended early or with a
		// terminal failure event. Surfacing this is essential — fabricating an
		// empty response would make a failed run "succeed" with empty output.
		return nil, NewModelBehaviorError("model stream ended without a completed response")
	}
	// Some backends (e.g. ChatGPT with store=false) return an empty Output
	// array in the completed event. Fall back to output assembled from
	// streaming deltas so the run produces a usable final result.
	if len(a.final.Output) == 0 {
		a.final.Output = a.items
	}
	return a.final, nil
}
