package session

import (
	"cmp"
	"context"
	"fmt"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/internal/oaiitems"
)

// RecoveryAction is what to do about a tool call a crashed run left without its output.
type RecoveryAction int

const (
	// RecoverSynthesizeError appends an error output for the call. The default
	// — spec §2.5h.
	RecoverSynthesizeError RecoveryAction = iota

	// RecoverRetry leaves the call dangling for the next run to execute again.
	RecoverRetry

	// RecoverLeave does nothing, for a caller repairing the session itself.
	RecoverLeave
)

// RecoveryPolicy decides how a session a crash damaged is repaired — spec §2.5h.
type RecoveryPolicy struct {
	// UnfinishedToolCall is the default action for a call with no output.
	UnfinishedToolCall RecoveryAction

	// RetrySafe reports whether a tool is safe to run again (RecoverRetry when
	// true); nil treats every tool as unsafe. History holds a tool NAME, so the
	// caller supplies it.
	RetrySafe func(toolName string) bool

	// Message renders the synthesized error output; nil uses a default that
	// says what happened.
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

// Recover repairs a session a crash left inconsistent: each function_call
// without an output gets a synthesized one appended, nothing rewritten — spec §2.5h.
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
			raw := oaiitems.FunctionCallOutput(callID, responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(msg)})
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

// defaultRecoveryMessage is the synthesized output when RecoveryPolicy.Message is nil.
func defaultRecoveryMessage(toolName, _ string) string {
	name := toolName
	name = cmp.Or(name, "the tool")
	return fmt.Sprintf("The run was interrupted while %s was executing, so its result was never "+
		"recorded. It may or may not have completed. Do not assume it succeeded; check or retry "+
		"if the outcome matters.", name)
}

// toolNamesByCallID maps each recorded call id to the tool it named.
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
