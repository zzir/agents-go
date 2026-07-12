package agents

import (
	"context"
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

// runStreamedLoop drives a streamed run. It shares runner.loop with Run —
// setting runner.sr switches the loop into streaming mode (raw event
// forwarding, run-item/agent-updated events, synchronous input guardrails);
// see the runner.sr field for the full list of differences.
func runStreamedLoop(ctx context.Context, startAgent *Agent, input any, opts RunOptions, sr *StreamedResult) (*RunResult, error) {
	r, modelInput, finishTrace, err := prepareRun(ctx, startAgent, input, opts)
	if err != nil {
		return nil, err
	}
	defer finishTrace()
	r.sr = sr
	return r.loop(ctx, startAgent, modelInput)
}

// emitStreamItem emits a run item's stream event to the consumer. A handoff
// call additionally emits a tool_called event wrapping the underlying function
// call, so a handoff surfaces as BOTH tool_called and handoff_requested —
// matching Python, which emits tool_called during model streaming and
// handoff_requested during side effects.
func (r *runner) emitStreamItem(ctx context.Context, it RunItem) {
	if hc, ok := it.(*HandoffCallItem); ok {
		r.sr.emit(ctx, &RunItemStreamEvent{Name: "tool_called", Item: &ToolCallItem{Agent: hc.Agent, Raw: hc.Raw}})
	}
	r.sr.emit(ctx, &RunItemStreamEvent{Name: runItemEventName(it), Item: it})
}

// streamOneModelCall streams a single model call, forwarding each raw event to
// the consumer and assembling the final ModelResponse from the completed event.
// The first event's arrival is stamped on the generation span as
// time_to_first_token_ms.
func (r *runner) streamOneModelCall(ctx context.Context, sr *StreamedResult, span *tracing.SpanHandle, model Model, req ModelRequest) (*ModelResponse, error) {
	var final *ModelResponse
	acc := &streamAccumulator{}
	start := time.Now()
	first := false
	for event, err := range model.StreamResponse(ctx, req) {
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		if !first {
			first = true
			span.Set("time_to_first_token_ms", time.Since(start).Milliseconds())
		}
		sr.emit(ctx, &RawResponsesStreamEvent{Data: event})
		acc.processEvent(event)
		if event.Type == "response.completed" {
			completed := event.AsResponseCompleted()
			final = &ModelResponse{
				Output:     completed.Response.Output,
				Usage:      usageFromStreamResponse(&completed.Response),
				ResponseID: completed.Response.ID,
			}
		}
	}
	if final == nil {
		// No response.completed event arrived: the stream ended early or with a
		// terminal failure event. Surfacing this is essential — fabricating an
		// empty response would make a failed run "succeed" with empty output.
		return nil, newModelBehaviorError("model stream ended without a completed response")
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
