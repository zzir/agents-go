package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// streamItem is one canonical output item under construction.
type streamItem struct {
	id  string
	typ string
}

// synthesizeStream translates the Messages SSE stream into canonical
// response.* events: deltas live, finished items at message_stop (decisions §5.49).
// ctx carries the run's diagnostic sink.
func synthesizeStream(ctx context.Context, stream *ssestream.Stream[ant.MessageStreamEventUnion], yield func(*agents.ResponseStreamEvent, error) bool, fingerprint string) {
	emit := func(ev agents.ResponseStreamEvent, err error) bool {
		if err != nil {
			yield(nil, err)
			return false
		}
		return yield(&ev, nil)
	}

	var acc ant.Message
	// itemOf maps a content block index to the output index of the item it
	// belongs to; consecutive text blocks share one message item.
	itemOf := map[int]int{}
	var items []streamItem
	terminalSent := false

	for stream.Next() {
		event := stream.Current()
		if err := accumulate(&acc, event); err != nil {
			yield(nil, fmt.Errorf("anthropic messages stream: accumulating %s: %w", event.Type, err))
			return
		}
		switch event.Type {
		case "message_start":
			recordDroppedThinking(ctx, event.Message)
			if !emit(modelkit.ResponseCreatedEvent(event.Message.ID)) {
				return
			}
		case "content_block_start":
			idx := int(event.Index)
			cb := event.ContentBlock
			var itemType, id, callID, name string
			switch cb.Type {
			case "text":
				// A text block directly after another continues that message.
				if prev, ok := itemOf[idx-1]; ok && items[prev].typ == "message" {
					itemOf[idx] = prev
					continue
				}
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
			itemOf[idx] = len(items)
			items = append(items, streamItem{id: id, typ: itemType})
			if !emit(modelkit.OutputItemAddedEvent(len(items)-1, itemType, id, callID, name)) {
				return
			}
		case "content_block_delta":
			oi, ok := itemOf[int(event.Index)]
			if !ok {
				yield(nil, agents.NewModelBehaviorError("anthropic: content_block_delta for unknown block index %d", event.Index))
				return
			}
			id := items[oi].id
			switch event.Delta.Type {
			case "text_delta":
				if !emit(modelkit.OutputTextDeltaEvent(id, oi, event.Delta.Text)) {
					return
				}
			case "thinking_delta":
				if !emit(modelkit.ReasoningTextDeltaEvent(id, oi, event.Delta.Thinking)) {
					return
				}
			case "input_json_delta":
				if !emit(modelkit.FunctionCallArgumentsDeltaEvent(id, oi, event.Delta.PartialJSON)) {
					return
				}
			case "signature_delta", "citations_delta":
				// Folded into the accumulated block; nothing incremental to show.
			}
		case "content_block_stop":
			// The finished item is emitted at message_stop, with the stop
			// reason known — see the doc comment.
			if idx := int(event.Index); idx < 0 || idx >= len(acc.Content) {
				yield(nil, agents.NewModelBehaviorError("anthropic: content_block_stop for unknown block index %d", idx))
				return
			}
		case "message_delta":
			// Accumulate copies OutputTokens but not OutputTokensDetails, and message_start
			// carries it as 0; without this a streamed call reports zero reasoning tokens.
			if event.Usage.JSON.OutputTokensDetails.Valid() {
				acc.Usage.OutputTokensDetails = event.Usage.OutputTokensDetails
			}
		case "message_stop":
			status, incompleteReason, err := statusFromStopReason(acc.StopReason)
			if err != nil {
				yield(nil, err)
				return
			}
			// Rebuilt from the accumulator in INDEX order: a stop-ordered history could
			// replay with thinking after text, which the API rejects.
			output, err := convertOutput(&acc, fingerprint)
			if err != nil {
				yield(nil, err)
				return
			}
			for i, item := range output {
				if !emit(modelkit.OutputItemDoneEvent(i, item)) {
					return
				}
			}
			final := modelkit.FinalResponse{ID: acc.ID, Output: output, Usage: responseUsage(acc.Usage), Model: string(acc.Model)}
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
	// is already complete and delivered.
	if terminalSent {
		return
	}
	if err := stream.Err(); err != nil {
		yield(nil, fmt.Errorf("anthropic messages stream: %w", err))
		return
	}
	// A clean SSE end without message_stop is a severed connection, surfaced
	// retryably rather than as a vague, unretryable early end.
	yield(nil, modelkit.TruncatedStreamError("anthropic messages stream"))
}

// accumulate folds event into acc, skipping the SDK's re-marshal at the two
// stop events while a tool_use holds JSON cut off by max_tokens: it would fail,
// and nothing read here comes from it, so the call surfaces as-is (spec §2.7e).
func accumulate(acc *ant.Message, event ant.MessageStreamEventUnion) error {
	switch event.Type {
	case "content_block_stop":
		if idx := int(event.Index); idx >= 0 && idx < len(acc.Content) && partialToolInput(acc.Content[idx]) {
			return nil
		}
	case "message_stop":
		if slices.ContainsFunc(acc.Content, partialToolInput) {
			return nil
		}
	}
	return acc.Accumulate(event)
}

// partialToolInput reports a tool_use whose accumulated input is not (yet) JSON.
func partialToolInput(block ant.ContentBlockUnion) bool {
	return block.Type == "tool_use" && !json.Valid(block.Input)
}

// blockItemID synthesizes a stable item id for anonymous blocks; convertOutput
// uses the same derivation so deltas and the finished item agree.
func blockItemID(msgID string, index int) string {
	return fmt.Sprintf("%s-%d", msgID, index)
}

// responseUsage maps Messages usage onto canonical accounting and owns this
// adapter's summation rule: InputTokens = uncached + cache-read + cache-write.
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

// diagnosticThinkingDropped is the diagnostic type of a replayed thinking
// block the API dropped — spec §2.15.
const diagnosticThinkingDropped agents.DiagnosticType = "thinking_dropped"

// recordDroppedThinking reports each thinking block the API dropped from the
// request, as message_start lists them; any other transformation is ignored.
func recordDroppedThinking(ctx context.Context, msg ant.Message) {
	// Raw, not Valid: the SDK does not know the field, so it is an extra one.
	raw := msg.JSON.ExtraFields["input_transformations"].Raw()
	if raw == "" {
		return
	}
	var entries []struct {
		Type   string `json:"type"`
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(raw), &entries) != nil {
		return
	}
	for _, e := range entries {
		if e.Type == string(diagnosticThinkingDropped) {
			agents.RecordDiagnostic(ctx, diagnosticThinkingDropped, nil, map[string]any{"path": e.Path, "reason": e.Reason})
		}
	}
}
