package session

import (
	"cmp"
	"context"
	"fmt"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/internal/oaiitems"
)

// RecoveryPolicy decides how a session a crash damaged is repaired — spec §2.5h.
type RecoveryPolicy struct {
	// RetrySafe reports whether a tool is safe to run again, leaving its call
	// dangling for the next run; nil treats every tool as unsafe. History holds
	// a tool NAME, so the caller supplies it.
	RetrySafe func(toolName string) bool
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

	pending := pendingCallIDs(entries)
	if len(pending) == 0 {
		return report, nil
	}
	report.UnfinishedCalls = pending

	names := CallToolNames(entries)
	var repair []Entry
	for _, callID := range pending {
		name := names[callID]
		if policy.RetrySafe != nil && policy.RetrySafe(name) {
			report.Retryable = append(report.Retryable, callID)
			continue
		}
		msg := recoveryMessage(name)
		raw := oaiitems.FunctionCallOutput(callID, responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(msg)})
		e, err := NewItemEntry(raw, Source{Type: SourceErrorHandler})
		if err != nil {
			return report, fmt.Errorf("recovering call %q: %w", callID, err)
		}
		e.Display = &ItemDisplay{Kind: DisplayToolOutput, CallID: callID, Output: msg, IsError: true}
		repair = append(repair, e)
		report.Repaired = append(report.Repaired, callID)
	}
	if len(repair) > 0 {
		if err := sess.Append(ctx, repair...); err != nil {
			return report, err
		}
	}
	return report, nil
}

// recoveryMessage is the synthesized output for a call the crash left unfinished.
func recoveryMessage(toolName string) string {
	return fmt.Sprintf("The run was interrupted while %s was executing, so its result was never "+
		"recorded. It may or may not have completed. Do not assume it succeeded; check or retry "+
		"if the outcome matters.", cmp.Or(toolName, "the tool"))
}
