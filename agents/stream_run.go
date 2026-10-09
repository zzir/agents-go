package agents

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/tracing"
)

// usageFromStreamResponse extracts token usage from a streamed final Response;
// a response carrying no usage block counts as zero requests, not one.
func usageFromStreamResponse(resp *responses.Response) *Usage {
	if !resp.JSON.Usage.Valid() {
		return NewUsage()
	}
	return UsageFromResponseUsage(resp.Usage)
}

// streamOneModelCall streams one model call, forwarding raw events and assembling
// the final ModelResponse; only Run takes this path, RunSync calls Respond once.
func (r *runner) streamOneModelCall(ctx context.Context, span *tracing.SpanHandle, model Model, req ModelRequest) (*ModelResponse, error) {
	asm := &responseAssembler{}
	start := time.Now()
	first := false
	// The stamp waits for the first delta (earlier events carry no token);
	// terminal events stamp as a fallback.
	stamp := func() {
		if !first {
			first = true
			span.Set("time_to_first_token_ms", time.Since(start).Milliseconds())
		}
	}
	for event, err := range model.StreamResponse(ctx, req) {
		if err != nil {
			r.recordPartialOutput(span, asm)
			return nil, err
		}
		if event == nil {
			continue
		}
		if strings.HasSuffix(event.Type, ".delta") ||
			event.Type == EventResponseCompleted || event.Type == EventResponseIncomplete {
			stamp()
		}
		if !r.emit(&RawResponsesStreamEvent{Data: event}) {
			return nil, errConsumerStopped
		}
		asm.observe(event)
	}
	resp, err := asm.result()
	if err != nil {
		r.recordPartialOutput(span, asm)
	}
	return resp, err
}

// recordPartialOutput puts what a failed stream had produced on its span — the
// items that completed, the text of the one in flight — gated like output.
func (r *runner) recordPartialOutput(span *tracing.SpanHandle, asm *responseAssembler) {
	if !r.traceIncludeSensitiveData() {
		return
	}
	if len(asm.items) > 0 {
		span.Set("output", slices.Clone(asm.items))
	}
	if asm.text.Len() > 0 {
		span.Set("partial_text", asm.text.String())
	}
}

// responseAssembler assembles the final ModelResponse from a raw Responses
// event stream, for the runner and NewStreamOnlyModel alike — see decisions §5.15.
type responseAssembler struct {
	final *ModelResponse
	items []OutputItem
	// text is the assistant text streamed so far: what a stream that fails
	// mid-message had produced.
	text strings.Builder
}

func (a *responseAssembler) observe(event *ResponseStreamEvent) {
	switch event.Type {
	case EventResponseOutputTextDelta:
		a.text.WriteString(event.Delta)
	case EventResponseOutputItemDone:
		// Collected as a fallback for backends (e.g. ChatGPT with store=false)
		// whose terminal event carries an empty Output array.
		done := event.AsResponseOutputItemDone()
		a.items = append(a.items, done.Item)
	case EventResponseCompleted:
		completed := event.AsResponseCompleted()
		a.final = &ModelResponse{
			Output:     completed.Response.Output,
			Usage:      usageFromStreamResponse(&completed.Response),
			ResponseID: completed.Response.ID,
			Model:      completed.Response.Model,
			Status:     string(completed.Response.Status),
		}
	case EventResponseIncomplete:
		// A cut-off response still arrived; assembled like any other — see spec §2.7e.
		inc := event.AsResponseIncomplete()
		a.final = &ModelResponse{
			Output:           inc.Response.Output,
			Usage:            usageFromStreamResponse(&inc.Response),
			ResponseID:       inc.Response.ID,
			Model:            inc.Response.Model,
			Status:           string(inc.Response.Status),
			IncompleteReason: inc.Response.IncompleteDetails.Reason,
		}
	}
}

func (a *responseAssembler) result() (*ModelResponse, error) {
	if a.final == nil {
		// The stream ended early or with a terminal failure event; never
		// fabricate an empty response.
		return nil, NewModelBehaviorError("model stream ended without a completed response")
	}
	// Some backends (ChatGPT with store=false) send an empty Output in the
	// completed event; fall back to the items assembled from the stream.
	if len(a.final.Output) == 0 {
		a.final.Output = a.items
	}
	return a.final, nil
}
