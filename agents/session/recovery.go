package session

import (
	"cmp"
	"context"
	"fmt"

	"github.com/openai/openai-go/v3/responses"
)

// RecoveryAction is what to do about a tool call a crashed run left without
// its output.
type RecoveryAction int

const (
	// RecoverSynthesizeError appends an error output for the call, so the
	// stored history loads again. The default — spec §2.5h.
	RecoverSynthesizeError RecoveryAction = iota

	// RecoverRetry leaves the call dangling for the next run to execute again;
	// only right for a tool that is safe to repeat.
	RecoverRetry

	// RecoverLeave does nothing, for a caller repairing the session itself.
	RecoverLeave
)

// RecoveryPolicy decides how a session damaged by a crash is repaired. It is
// the counterpart of RunState (a paused run), not a replacement: this handles
// a process that died mid-turn and left only what was written — spec §2.5h.
type RecoveryPolicy struct {
	// UnfinishedToolCall is the default action for a call with no output.
	UnfinishedToolCall RecoveryAction

	// RetrySafe reports whether a tool is safe to run again, overriding the
	// default with RecoverRetry when true. Nil treats every tool as unsafe. The
	// caller supplies it: the stored history holds a tool NAME, not the tool.
	RetrySafe func(toolName string) bool

	// Message renders the synthesized error output. Nil uses a default that
	// tells the model plainly what happened, so it can decide whether to try
	// again rather than treating the absence as a result.
	Message func(toolName, callID string) string
}

// RecoveryReport describes what a recovery pass found and did.
type RecoveryReport struct {
	// UnfinishedCalls are the call ids that had no output.
	UnfinishedCalls []string
	// Repaired are the calls given a synthesized error output.
	Repaired []string
	// Retryable are the calls left dangling for a retry-safe tool.
	Retryable []string
}

// NeedsRecovery reports whether anything was found.
func (r RecoveryReport) NeedsRecovery() bool { return len(r.UnfinishedCalls) > 0 }

// Recover repairs a session left inconsistent by a crash: a function_call
// with no output, which makes the history unloadable. The repair is an
// append of synthesized outputs; nothing is rewritten — spec §2.5h.
func Recover(ctx context.Context, sess *Session, policy RecoveryPolicy) (RecoveryReport, error) {
	var report RecoveryReport
	if sess == nil {
		return report, nil
	}
	entries, err := sess.ContextEntries(ctx, Cursor{})
	if err != nil {
		return report, err
	}

	state := ReduceState(entries)
	if len(state.PendingCallIDs) == 0 {
		return report, nil
	}
	report.UnfinishedCalls = state.PendingCallIDs

	names := toolNamesByCallID(entries)
	message := policy.Message
	if message == nil {
		message = defaultRecoveryMessage
	}

	var repair []Entry
	for _, callID := range state.PendingCallIDs {
		name := names[callID]
		action := policy.UnfinishedToolCall
		if policy.RetrySafe != nil && policy.RetrySafe(name) {
			action = RecoverRetry
		}
		switch action {
		case RecoverRetry:
			report.Retryable = append(report.Retryable, callID)
		case RecoverLeave:
			// The caller is handling it.
		default:
			msg := message(name, callID)
			raw := responses.ResponseInputItemParamOfFunctionCallOutput(callID, msg)
			e, err := NewItemEntry(raw, Source{Type: SourceErrorHandler})
			if err != nil {
				return report, fmt.Errorf("recovering call %q: %w", callID, err)
			}
			e.Display = &ItemDisplay{Kind: DisplayToolOutput, CallID: callID, Output: msg, IsError: true}
			repair = append(repair, e)
			report.Repaired = append(report.Repaired, callID)
		}
	}
	if len(repair) > 0 {
		if err := sess.Append(ctx, repair...); err != nil {
			return report, err
		}
	}
	return report, nil
}

// defaultRecoveryMessage tells the model what happened rather than leaving a
// blank result, which it would otherwise read as "the tool returned nothing".
func defaultRecoveryMessage(toolName, _ string) string {
	name := toolName
	name = cmp.Or(name, "the tool")
	return fmt.Sprintf("The run was interrupted while %s was executing, so its result was never "+
		"recorded. It may or may not have completed. Do not assume it succeeded; check or retry "+
		"if the outcome matters.", name)
}

// toolNamesByCallID maps each recorded call id to the tool it named, so a
// synthesized output can say which tool was interrupted.
func toolNamesByCallID(entries []Entry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		if e.Kind != EntryKindItem {
			continue
		}
		if e.Display != nil && e.Display.CallID != "" && e.Display.ToolName != "" {
			out[e.Display.CallID] = e.Display.ToolName
			continue
		}
		if p := ProbeItem(e.Item); p.Type == "function_call" && p.CallID != "" && p.Name != "" {
			out[p.CallID] = p.Name
		}
	}
	return out
}
