package anthropic

import (
	"fmt"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// synthesizeStream translates the Messages SSE stream into canonical
// response.* events, yielding them to the consumer.
//
// The SDK's Message accumulator is the source of truth for final content:
// deltas are forwarded as presentation events the moment they arrive, but the
// items in output_item.done and the terminal event are converted from the
// accumulated blocks — the same code path as the blocking response — so the
// two paths cannot drift.
func synthesizeStream(stream *ssestream.Stream[ant.MessageStreamEventUnion], yield func(*agents.ResponseStreamEvent, error) bool) {
	emit := func(ev agents.ResponseStreamEvent, err error) bool {
		if err != nil {
			yield(nil, err)
			return false
		}
		return yield(&ev, nil)
	}

	var acc ant.Message
	itemIDs := map[int]string{}
	terminalSent := false

	for stream.Next() {
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			yield(nil, fmt.Errorf("anthropic messages stream: accumulating %s: %w", event.Type, err))
			return
		}
		switch event.Type {
		case "message_start":
			if !emit(modelkit.ResponseCreatedEvent(event.Message.ID)) {
				return
			}
		case "content_block_start":
			idx := int(event.Index)
			cb := event.ContentBlock
			var itemType, id, callID, name string
			switch cb.Type {
			case "text":
				itemType, id = "message", blockItemID(acc.ID, idx)
			case "thinking", "redacted_thinking":
				itemType, id = "reasoning", blockItemID(acc.ID, idx)
			case "tool_use":
				itemType, id, callID, name = "function_call", cb.ID, cb.ID, cb.Name
			default:
				yield(nil, agents.NewModelBehaviorError(
					"anthropic: response contained an unexpected content block of type %q", cb.Type))
				return
			}
			itemIDs[idx] = id
			if !emit(modelkit.OutputItemAddedEvent(idx, itemType, id, callID, name)) {
				return
			}
		case "content_block_delta":
			idx := int(event.Index)
			switch event.Delta.Type {
			case "text_delta":
				if !emit(modelkit.OutputTextDeltaEvent(itemIDs[idx], idx, event.Delta.Text)) {
					return
				}
			case "thinking_delta":
				if !emit(modelkit.ReasoningTextDeltaEvent(itemIDs[idx], idx, event.Delta.Thinking)) {
					return
				}
			case "input_json_delta":
				if !emit(modelkit.FunctionCallArgumentsDeltaEvent(itemIDs[idx], idx, event.Delta.PartialJSON)) {
					return
				}
			case "signature_delta", "citations_delta":
				// Folded into the accumulated block; nothing incremental to show.
			}
		case "content_block_stop":
			idx := int(event.Index)
			if idx < 0 || idx >= len(acc.Content) {
				yield(nil, agents.NewModelBehaviorError("anthropic: content_block_stop for unknown block index %d", idx))
				return
			}
			// stop_reason usually lands AFTER the per-block stops, so a
			// refusal's mid-stream item.done still says output_text (and may
			// name a tool call); the terminal rebuild below has the real stop
			// reason, collapses a refusal to its single refusal item, and is
			// what the runner reads.
			item, err := blockToItem(acc.ID, idx, acc.Content[idx])
			if err != nil {
				yield(nil, err)
				return
			}
			if !emit(modelkit.OutputItemDoneEvent(idx, item)) {
				return
			}
		case "message_delta":
			// Stop reason and output usage fold into acc — except the output
			// token breakdown, which Accumulate drops (it copies OutputTokens
			// and the cache fields from the delta but not OutputTokensDetails),
			// and message_start carries it as 0. Without this, streamed calls
			// would always report zero reasoning tokens while blocking calls
			// report the real count.
			if event.Usage.JSON.OutputTokensDetails.Valid() {
				acc.Usage.OutputTokensDetails = event.Usage.OutputTokensDetails
			}
		case "message_stop":
			status, incompleteReason, err := statusFromStopReason(acc.StopReason)
			if err != nil {
				yield(nil, err)
				return
			}
			// The terminal output is rebuilt from the accumulator in INDEX
			// order, not content_block_stop arrival order: the protocol only
			// guarantees start events are index-ordered — deltas and stops for
			// still-open blocks may interleave — and a stop-ordered history
			// could replay with thinking after text, which the API rejects.
			// The refusal collapse and the block conversion share one
			// implementation with the blocking path — convertOutput reads
			// the accumulated message directly.
			output, err := convertOutput(&acc)
			if err != nil {
				yield(nil, err)
				return
			}
			final := modelkit.FinalResponse{ID: acc.ID, Output: output, Usage: responseUsage(acc.Usage)}
			if status == "incomplete" {
				if !emit(modelkit.IncompleteEvent(final, incompleteReason)) {
					return
				}
			} else if !emit(modelkit.CompletedEvent(final)) {
				return
			}
			terminalSent = true
		}
	}
	// A transport error AFTER the terminal event is not surfaced: the response
	// is already complete and delivered, and failing the call now would throw
	// it away over a connection that had nothing left to say.
	if terminalSent {
		return
	}
	if err := stream.Err(); err != nil {
		yield(nil, fmt.Errorf("anthropic messages stream: %w", err))
		return
	}
	// The SSE layer reports a clean end but message_stop never arrived: the
	// connection was severed at an event boundary and the response is cut
	// off. Surfaced retryably (modelkit.TruncatedStreamError wraps
	// io.ErrUnexpectedEOF) instead of ending the stream silently, which would
	// leave the runner to report a vague, unretryable "ended without a
	// completed response".
	yield(nil, modelkit.TruncatedStreamError("anthropic messages stream"))
}

// blockItemID synthesizes a stable item id for blocks the API leaves
// anonymous. It must agree between the delta events and the finished item —
// blockToItem uses the same derivation.
func blockItemID(msgID string, index int) string {
	return fmt.Sprintf("%s-%d", msgID, index)
}

// responseUsage maps Messages usage onto the canonical terminal-event
// accounting. It owns the summation rule for this adapter — canonical
// InputTokens is uncached + cache-read + cache-write — and the blocking path's
// usageFromMessage (convert.go) is built on it rather than repeating it.
func responseUsage(u ant.Usage) modelkit.ResponseUsage {
	in := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return modelkit.ResponseUsage{
		InputTokens:      in,
		OutputTokens:     u.OutputTokens,
		TotalTokens:      in + u.OutputTokens,
		CachedTokens:     u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		ReasoningTokens:  u.OutputTokensDetails.ThinkingTokens,
	}
}

func settingsToolChoice(s *agents.ModelSettings) agents.ToolChoice {
	if s == nil {
		return ""
	}
	return s.ToolChoice
}

func settingsParallel(s *agents.ModelSettings) *bool {
	if s == nil {
		return nil
	}
	return s.ParallelToolCalls
}
