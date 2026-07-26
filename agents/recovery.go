package agents

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
)

// RecoveryAction is what to do about a tool call a crashed run left without
// its output.
type RecoveryAction int

const (
	// RecoverSynthesizeError appends an error output for the call, so the
	// stored history is self-consistent again. It is the default because a
	// dangling call is not merely untidy: the Responses API rejects a history
	// containing a function_call with no matching function_call_output, so the
	// session cannot be loaded at all until one exists.
	RecoverSynthesizeError RecoveryAction = iota

	// RecoverRetry leaves the call dangling for the next run to execute again.
	// It is only ever right for a tool that says it is safe to repeat — the
	// SDK cannot know whether the crashed call already sent the email.
	RecoverRetry

	// RecoverLeave does nothing, for a caller repairing the session itself.
	RecoverLeave
)

// RecoveryPolicy decides how a session damaged by a crash is repaired.
//
// It is the counterpart of RunState, not a replacement: RunState handles a run
// that paused on purpose and knows exactly where it was, while this handles a
// process that died mid-turn and left only what had been written. Different
// entry points, and a session can need both.
//
// safePersistBoundary already keeps a dangling call out of a session on every
// ordinary exit. It cannot help when the process is killed.
type RecoveryPolicy struct {
	// UnfinishedToolCall is the default action for a call with no output.
	UnfinishedToolCall RecoveryAction

	// RetrySafe reports whether a tool is safe to run again, overriding the
	// default action with RecoverRetry when it returns true.
	//
	// The caller supplies it because only the caller knows the agent: the SDK
	// sees a tool NAME in the stored history, not the tool. Nil treats every
	// tool as unsafe, which is the assumption to make when nobody has said
	// otherwise.
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

// RecoverSession repairs a session left inconsistent by a crash.
//
// A run killed between issuing a tool call and recording its output leaves a
// function_call with no function_call_output. The Responses API rejects that
// history outright, so the session is not merely damaged — it is unloadable,
// and every later attempt to continue the conversation fails the same way.
//
// The repair is an APPEND, like everything else: the synthesized outputs are
// added, nothing is rewritten, and the record of what actually happened stays
// intact.
func RecoverSession(ctx context.Context, sess *Session, policy RecoveryPolicy) (RecoveryReport, error) {
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

	var repair []SessionEntry
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
			item := newFunctionCallOutputItem(nil, callID, message(name, callID))
			item.IsError = true
			e, err := EntryFromRunItem(item, "")
			if err != nil {
				return report, fmt.Errorf("recovering call %q: %w", callID, err)
			}
			e.Source = Source{Type: SourceErrorHandler}
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
func toolNamesByCallID(entries []SessionEntry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		if e.Kind != EntryKindItem {
			continue
		}
		if e.Display != nil && e.Display.CallID != "" && e.Display.ToolName != "" {
			out[e.Display.CallID] = e.Display.ToolName
			continue
		}
		if id, isCall, _ := entryCallID(e); isCall && id != "" {
			var probe struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(e.Item, &probe) == nil && probe.Name != "" {
				out[id] = probe.Name
			}
		}
	}
	return out
}
