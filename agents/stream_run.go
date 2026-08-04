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
	var final *ModelResponse
	acc := &streamAccumulator{}
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
		acc.processEvent(event)
		switch event.Type {
		case "response.completed":
			completed := event.AsResponseCompleted()
			final = &ModelResponse{
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
			final = &ModelResponse{
				Output:           inc.Response.Output,
				Usage:            usageFromStreamResponse(&inc.Response),
				ResponseID:       inc.Response.ID,
				Status:           string(inc.Response.Status),
				IncompleteReason: inc.Response.IncompleteDetails.Reason,
			}
		}
	}
	if final == nil {
		// No response.completed event arrived: the stream ended early or with a
		// terminal failure event. Surfacing this is essential — fabricating an
		// empty response would make a failed run "succeed" with empty output.
		return nil, NewModelBehaviorError("model stream ended without a completed response")
	}
	// Some backends (e.g. ChatGPT with store=false) return an empty Output
	// array in the completed event. Fall back to output assembled from
	// streaming deltas so the run produces a usable final result.
	if len(final.Output) == 0 {
		final.Output = acc.buildOutput()
	}
	return final, nil
}

// streamAccumulator collects output items from streaming events so they can
// be used as a fallback when the completed response has an empty Output array.
type streamAccumulator struct {
	items []TResponseOutputItem
}

func (a *streamAccumulator) processEvent(event *TResponseStreamEvent) {
	if event.Type == "response.output_item.done" {
		done := event.AsResponseOutputItemDone()
		a.items = append(a.items, done.Item)
	}
}

func (a *streamAccumulator) buildOutput() []TResponseOutputItem {
	return a.items
}
