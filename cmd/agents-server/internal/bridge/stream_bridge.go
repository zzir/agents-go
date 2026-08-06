package bridge

import (
	"errors"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// The stream bridge translates in one direction only: SDK run events into
// protocol envelopes. Nothing here decides anything about the run, which is
// why it sits apart from the execution pipeline in runner.go.

// drainStream forwards a streamed run's events to the hub and accumulates the
// still-unpersisted reasoning/text so an abort can persist them as
// display-only annotations (a cancel during the thinking phase still shows what
// the model was doing). A terminal error on the event channel stops
// consumption; the caller reads the run's outcome from FinalResult.
//
// The buffer resets on ItemsPersistedEvent — the SDK's own statement that
// everything the stream showed so far is in the store — so what remains is
// exactly what a reload could not recover. Persist timing is the SDK's to
// announce, not this bridge's to reverse-engineer from raw response events;
// deltas cannot race the reset because the SDK persists between model calls,
// where no delta is in flight.
func (r *Runner) drainStream(stream agents.RunStream, runID string, send func(string, any)) (res *agents.RunResult, streamedText, streamedReasoning string, runErr error) {
	var text, reasoning strings.Builder
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
			// Except the diagnostics: a run that answered after three retries
			// or on a fallback model looks identical to one that answered
			// first time, and the difference is what explains the latency.
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
		r.handleStreamEvent(event, runID, send)
	}
	return res, text.String(), reasoning.String(), runErr
}

// runErrorFor builds the run.error for a terminal run failure. The code comes
// from the SDK (agents.CodeOf) so this stays correct as the SDK's vocabulary
// grows — there is deliberately no mapping table here. An error the SDK did not
// classify keeps the caller's transport-level fallback code.
//
// A guardrail tripwire additionally carries the guardrail name and the stage it
// fired at, which no code can express: the UI renders "blocked by guardrail X"
// instead of a generic red error and, on an output trip, marks the answer that
// already streamed as retracted.
func runErrorFor(runID string, err error, fallback string) protocol.RunError {
	e := protocol.RunError{RunID: runID, Code: fallback, Message: err.Error()}
	if code := agents.CodeOf(err); code != agents.CodeUnknown {
		e.Code = string(code)
	}
	var tw *agents.GuardrailTripwireError
	if errors.As(err, &tw) {
		e.Guardrail = tw.Result.Guardrail.Name
		e.Stage = string(tw.Stage())
	}
	return e
}

func (r *Runner) handleStreamEvent(event agents.StreamEvent, runID string, send func(string, any)) {
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
			// The completed turn text, authoritative over the run.step deltas
			// that previewed it. Interim messages between tool calls only exist
			// as deltas plus this event — resumed segments and backends that
			// stream no deltas rely on it entirely.
			if e.Item.Kind == agents.ItemMessage {
				if text := e.Item.Text(); text != "" {
					send(protocol.EventRunMessage, protocol.RunMessage{RunID: runID, Text: text, ItemID: rawItemID(e.Item)})
				}
			}
		case "reasoning_item_created":
			// The completed thinking block, authoritative over the run.reasoning
			// deltas that previewed it — and the only thinking signal when the
			// backend streams no reasoning deltas or the segment was resumed.
			if e.Item.Kind == agents.ItemReasoning {
				if text := e.Item.Text(); text != "" {
					send(protocol.EventRunReasoningItem, protocol.RunReasoningItem{RunID: runID, Text: text, ItemID: rawItemID(e.Item)})
				}
			}
		case "tool_called":
			if e.Item.Kind == agents.ItemToolCall {
				// The SDK emits tool_called for a handoff too (wrapping the
				// transfer_to_X call, marked IsHandoff); it has no tool_output,
				// so a run.tool_call here would leave a tool card spinning
				// forever. run.handoff already conveys the transfer, so drop it.
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
				// The display rendering, not %v: a multimodal output is a
				// content list, and Go syntax for it would not match what
				// the same item reads back as from the stored session. The
				// rest of the display travels too, so the live card carries
				// the same Title/Summary/Renderer/IsError a reload rebuilds
				// from the stored entry.
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
					RunID: runID,
					From:  e.Item.Agent.Name,
				})
			}
		case "injected_input_created":
			// Input injected into a live run (run.inject) is a USER entry, and
			// no live event carries one: run.started.Input covers only the
			// prompt a run begins with, and PROTOCOL.md F2's run.entry — the
			// event that will — has not shipped. The client that injected it
			// already has the text, and the SDK persists the item, so every
			// other connection picks it up on its next history load. Named
			// here rather than left out of the switch: the drop is a decision,
			// and this is where run.entry lands once it ships.
		case "handoff_occured":
			if e.Item.Kind == agents.ItemHandoffOutput && e.Item.HandoffFrom != nil && e.Item.HandoffTo != nil {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID: runID,
					From:  e.Item.HandoffFrom.Name,
					To:    e.Item.HandoffTo.Name,
				})
			}
		}

	case *agents.ToolProgressEvent:
		// A tool that runs for two minutes leaves the UI with nothing but a
		// spinner otherwise. This is not the tool's answer — that arrives as
		// run.tool_result — so the client renders it as live output.
		send(protocol.EventRunToolProgress, protocol.RunToolProgress{
			RunID:    runID,
			CallID:   e.CallID,
			ToolName: e.ToolName,
			Delta:    e.Result.Text(),
			Renderer: e.Result.Display,
		})

	case *agents.AgentUpdatedStreamEvent:
		send(protocol.EventRunAgentStart, protocol.RunAgentStart{
			RunID:     runID,
			AgentName: e.NewAgent.Name,
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
