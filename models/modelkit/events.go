package modelkit

import (
	"encoding/json"
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// EventFromJSON decodes canonical wire JSON into a stream event, stamping the
// raw bytes exactly as OutputItemFromJSON does for items.
func EventFromJSON(raw []byte) (agents.TResponseStreamEvent, error) {
	var ev agents.TResponseStreamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ev, fmt.Errorf("modelkit: decoding stream event: %w", err)
	}
	return ev, nil
}

// marshalEvent builds an event from a marshalable payload.
func marshalEvent(payload any) (agents.TResponseStreamEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return agents.TResponseStreamEvent{}, err
	}
	return EventFromJSON(raw)
}

// Sequence numbers are not part of the adapter contract: the runner and the
// server bridge key on event types and item ids, never on sequence_number, and
// a translated stream has no upstream numbering to preserve. Every synthesized
// event carries 0, matching the agentstest fake.

// ResponseCreatedEvent synthesizes the response.created event that opens a
// stream. Consumers use it as the "a response is now in flight" signal, so an
// adapter should emit it as its first event.
func ResponseCreatedEvent(responseID string) (agents.TResponseStreamEvent, error) {
	return marshalEvent(struct {
		Type           string `json:"type"`
		SequenceNumber int    `json:"sequence_number"`
		Response       struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Output []any  `json:"output"`
		} `json:"response"`
	}{Type: "response.created", Response: struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []any  `json:"output"`
	}{ID: responseID, Status: "in_progress", Output: []any{}}})
}

// OutputItemAddedEvent synthesizes response.output_item.added announcing that
// item construction has begun. itemType is the canonical type ("message",
// "reasoning", "function_call"); name is the tool name and callID the call id
// for function_call items, ignored otherwise.
func OutputItemAddedEvent(outputIndex int, itemType, itemID, callID, name string) (agents.TResponseStreamEvent, error) {
	item := map[string]any{"id": itemID, "type": itemType, "status": "in_progress"}
	switch itemType {
	case "message":
		item["role"] = "assistant"
		item["content"] = []any{}
	case "reasoning":
		item["summary"] = []any{}
	case "function_call":
		item["call_id"] = callID
		item["name"] = name
		item["arguments"] = ""
	}
	return marshalEvent(map[string]any{
		"type":            "response.output_item.added",
		"sequence_number": 0,
		"output_index":    outputIndex,
		"item":            item,
	})
}

// OutputTextDeltaEvent synthesizes response.output_text.delta — the event
// streaming text consumers (including the agents-server UI) render from.
func OutputTextDeltaEvent(itemID string, outputIndex int, delta string) (agents.TResponseStreamEvent, error) {
	return marshalEvent(map[string]any{
		"type":            "response.output_text.delta",
		"sequence_number": 0,
		"item_id":         itemID,
		"output_index":    outputIndex,
		"content_index":   0,
		"delta":           delta,
		"logprobs":        []any{},
	})
}

// ReasoningTextDeltaEvent synthesizes response.reasoning_text.delta for
// incremental raw reasoning text, matching where ReasoningItem puts the final
// text (content, not summary).
func ReasoningTextDeltaEvent(itemID string, outputIndex int, delta string) (agents.TResponseStreamEvent, error) {
	return marshalEvent(map[string]any{
		"type":            "response.reasoning_text.delta",
		"sequence_number": 0,
		"item_id":         itemID,
		"output_index":    outputIndex,
		"content_index":   0,
		"delta":           delta,
	})
}

// FunctionCallArgumentsDeltaEvent synthesizes
// response.function_call_arguments.delta for incremental tool-call arguments.
func FunctionCallArgumentsDeltaEvent(itemID string, outputIndex int, delta string) (agents.TResponseStreamEvent, error) {
	return marshalEvent(map[string]any{
		"type":            "response.function_call_arguments.delta",
		"sequence_number": 0,
		"item_id":         itemID,
		"output_index":    outputIndex,
		"delta":           delta,
	})
}

// OutputItemDoneEvent synthesizes response.output_item.done carrying the
// finished item. The runner's stream accumulator collects exactly these, so an
// adapter must emit one per output item, in order.
func OutputItemDoneEvent(outputIndex int, item agents.TResponseOutputItem) (agents.TResponseStreamEvent, error) {
	raw := item.RawJSON()
	if raw == "" {
		return agents.TResponseStreamEvent{}, fmt.Errorf("modelkit: output item has no raw JSON (construct it with the modelkit item builders)")
	}
	return marshalEvent(map[string]any{
		"type":            "response.output_item.done",
		"sequence_number": 0,
		"output_index":    outputIndex,
		"item":            json.RawMessage(raw),
	})
}

// ResponseUsage is the canonical token accounting for a synthesized terminal
// event. InputTokens is the TOTAL input count — cached reads and cache writes
// included — matching Responses semantics; CachedTokens and CacheWriteTokens
// are informational subsets of it. An adapter whose backend reports uncached
// input separately (as Anthropic does) must add the parts together.
type ResponseUsage struct {
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CachedTokens     int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// FinalResponse is everything a terminal stream event carries.
type FinalResponse struct {
	ID     string
	Output []agents.TResponseOutputItem
	Usage  ResponseUsage
}

// terminalEvent builds response.completed / response.incomplete.
func terminalEvent(eventType, status string, fr FinalResponse, incompleteReason string) (agents.TResponseStreamEvent, error) {
	items := make([]json.RawMessage, 0, len(fr.Output))
	for i := range fr.Output {
		raw := fr.Output[i].RawJSON()
		if raw == "" {
			return agents.TResponseStreamEvent{}, fmt.Errorf("modelkit: output item %d has no raw JSON (construct it with the modelkit item builders)", i)
		}
		items = append(items, json.RawMessage(raw))
	}
	response := map[string]any{
		"id":     fr.ID,
		"status": status,
		"output": items,
		"usage": map[string]any{
			"input_tokens":  fr.Usage.InputTokens,
			"output_tokens": fr.Usage.OutputTokens,
			"total_tokens":  fr.Usage.TotalTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens":      fr.Usage.CachedTokens,
				"cache_write_tokens": fr.Usage.CacheWriteTokens,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": fr.Usage.ReasoningTokens,
			},
		},
	}
	if incompleteReason != "" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	return marshalEvent(map[string]any{
		"type":            eventType,
		"sequence_number": 0,
		"response":        response,
	})
}

// CompletedEvent synthesizes the response.completed terminal event. The runner
// assembles its final ModelResponse from this event, so the output list and
// usage here are what the run records — deltas are presentation only.
func CompletedEvent(fr FinalResponse) (agents.TResponseStreamEvent, error) {
	return terminalEvent("response.completed", "completed", fr, "")
}

// IncompleteEvent synthesizes the response.incomplete terminal event. Use
// reason "max_output_tokens" for a response cut off at the output-token limit:
// that is the one incomplete reason the runner treats as recoverable
// truncation (agents.ModelResponse.Truncated) rather than a failure.
func IncompleteEvent(fr FinalResponse, reason string) (agents.TResponseStreamEvent, error) {
	if reason == "" {
		return agents.TResponseStreamEvent{}, fmt.Errorf("modelkit: IncompleteEvent requires a reason")
	}
	return terminalEvent("response.incomplete", "incomplete", fr, reason)
}
