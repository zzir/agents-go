package bridge

import (
	"errors"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// The stream bridge translates one way only: SDK run events into protocol
// envelopes. It decides nothing about the run (that is runner.go's).

// drainStream forwards a run's events to the hub, buffering unpersisted reasoning and
// text for an abort; the buffer resets on ItemsPersistedEvent, which no delta races.
func (r *Runner) drainStream(stream agents.RunStream, runID string, send func(string, any), partial *streamedPartial, agentIDs map[string]string) (res *agents.RunResult, runErr error) {
	text, reasoning := &partial.text, &partial.reasoning
	for event, err := range stream {
		if err != nil {
			runErr = err
			break
		}
		if _, ok := event.(*agents.ItemsPersistedEvent); ok {
			text.Reset()
			reasoning.Reset()
			continue
		}
		if done, ok := event.(*agents.RunCompletedEvent); ok {
			// The stream's terminal event carries the finished run; it is the
			// loop's own bookkeeping and not something the client renders.
			res = done.Result
			// Except the diagnostics: a retried or fallback answer looks like a
			// first-time one, and the difference explains the latency.
			for _, d := range res.Diagnostics {
				send(protocol.EventRunDiagnostic, protocol.RunDiagnostic{
					RunID:   runID,
					Type:    string(d.Type),
					Code:    string(d.Code),
					Message: d.Message,
					Details: d.Details,
				})
			}
			continue
		}
		if raw, ok := event.(*agents.RawResponsesStreamEvent); ok && raw.Data != nil {
			switch raw.Data.Type {
			case "response.output_text.delta":
				text.WriteString(raw.Data.Delta)
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				reasoning.WriteString(raw.Data.Delta)
			}
		}
		r.handleStreamEvent(event, runID, send, agentIDs)
	}
	return res, runErr
}

// streamedPartial is what the stream showed since the SDK last persisted —
// held by the caller so a panic mid-stream can still record it.
type streamedPartial struct{ text, reasoning strings.Builder }

func (p *streamedPartial) Text() string      { return p.text.String() }
func (p *streamedPartial) Reasoning() string { return p.reasoning.String() }

// runErrorFor builds the run.error: the SDK's code when it classified err, else the
// caller's transport fallback; a guardrail tripwire adds its name and stage.
func runErrorFor(runID string, err error, fallback string) protocol.RunError {
	e := protocol.RunError{RunID: runID, Code: fallback, Message: err.Error()}
	if code := agents.CodeOf(err); code != agents.CodeUnknown {
		e.Code = string(code)
	}
	if tw, ok := errors.AsType[*agents.GuardrailTripwireError](err); ok {
		e.Guardrail = tw.Result.Guardrail.Name
		e.Stage = string(tw.Stage())
	}
	return e
}

// agentIDs maps built agent names to config ids (BuildResult.AgentIDs); the
// events that announce an agent by name carry the id along for its avatar.
func (r *Runner) handleStreamEvent(event agents.StreamEvent, runID string, send func(string, any), agentIDs map[string]string) {
	switch e := event.(type) {
	case *agents.RawResponsesStreamEvent:
		if e.Data == nil {
			return
		}
		switch e.Data.Type {
		case "response.output_text.delta":
			if e.Data.Delta != "" {
				send(protocol.EventRunStep, protocol.RunStep{RunID: runID, Delta: e.Data.Delta})
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if e.Data.Delta != "" {
				send(protocol.EventRunReasoning, protocol.RunReasoning{RunID: runID, Delta: e.Data.Delta})
			}
		}

	case *agents.RunItemStreamEvent:
		switch e.Name {
		case "message_output_created":
			// The completed turn text, authoritative over the run.step deltas;
			// resumed segments and delta-less backends rely on it entirely.
			if e.Item.Kind == agents.ItemMessage {
				if text := e.Item.Text(); text != "" {
					send(protocol.EventRunMessage, protocol.RunMessage{RunID: runID, Text: text, ItemID: rawItemID(e.Item)})
				}
			}
		case "reasoning_item_created":
			// The completed thinking block, authoritative over run.reasoning
			// deltas — and the only signal when a backend streams none.
			if e.Item.Kind == agents.ItemReasoning {
				if text := e.Item.Text(); text != "" {
					send(protocol.EventRunReasoningItem, protocol.RunReasoningItem{RunID: runID, Text: text, ItemID: rawItemID(e.Item)})
				}
			}
		case "tool_called":
			if e.Item.Kind == agents.ItemToolCall {
				// A handoff's tool_called (IsHandoff) never gets a tool_output,
				// so its card would spin forever; run.handoff already conveys it.
				if e.Item.IsHandoff {
					return
				}
				fc := e.Item.FunctionCall()
				send(protocol.EventRunToolCall, protocol.RunToolCall{
					RunID:      runID,
					ToolCallID: fc.CallID,
					ToolName:   fc.Name,
					Arguments:  fc.Arguments,
				})
			}
		case "tool_output":
			if e.Item.Kind == agents.ItemToolCallOutput {
				// The display rendering, not %v: a multimodal output is a content
				// list, and the live card must match what a reload rebuilds.
				d := e.Item.Display()
				send(protocol.EventRunToolResult, protocol.RunToolResult{
					RunID:      runID,
					ToolCallID: e.Item.CallID(),
					Output:     d.Output,
					Title:      d.Title,
					Summary:    d.Summary,
					Renderer:   d.Renderer,
					IsError:    d.IsError,
					Extra:      d.Extra,
				})
			}
		case "handoff_requested":
			if e.Item.Kind == agents.ItemHandoffCall && e.Item.Agent != nil {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID:  runID,
					From:   e.Item.Agent.Name,
					FromID: agentIDs[e.Item.Agent.Name],
				})
			}
		case "injected_input_created":
			// An injected input is a USER entry no live event carries; the SDK
			// persists it and every other connection reads it on its next load.
		case "handoff_occured":
			if e.Item.Kind == agents.ItemHandoffOutput && e.Item.HandoffFrom != nil && e.Item.HandoffTo != nil {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID:  runID,
					From:   e.Item.HandoffFrom.Name,
					To:     e.Item.HandoffTo.Name,
					FromID: agentIDs[e.Item.HandoffFrom.Name],
					ToID:   agentIDs[e.Item.HandoffTo.Name],
				})
			}
		}

	case *agents.ToolProgressEvent:
		// Live output of a long-running tool; the answer arrives separately
		// as run.tool_result.
		send(protocol.EventRunToolProgress, protocol.RunToolProgress{
			RunID:    runID,
			CallID:   e.CallID,
			ToolName: e.ToolName,
			Delta:    e.Result.Text(),
			Renderer: e.Result.Display,
		})

	case *agents.AgentUpdatedStreamEvent:
		send(protocol.EventRunAgentStart, protocol.RunAgentStart{
			RunID:         runID,
			AgentName:     e.NewAgent.Name,
			AgentConfigID: agentIDs[e.NewAgent.Name],
		})
	}
}

// rawItemID returns the model-assigned id of the item's raw form, or "" when
// the item carries none (a rebuilt or synthesized item).
func rawItemID(it *agents.RunItem) string {
	if it.Raw == nil {
		return ""
	}
	return it.Raw.ID
}
