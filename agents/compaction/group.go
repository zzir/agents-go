// Package compaction shrinks a session's history so a long conversation keeps
// fitting in a model's context window.
//
// The unit of work is a group, not an entry. A function call and its output
// belong together — sending one without the other is rejected by the API — and
// so does a reasoning block and the tool call it precedes. Grouping makes those
// pairings structural: once entries are grouped, cutting through a pair is not
// something a strategy can do wrong, because a strategy only ever includes or
// excludes whole groups.
package compaction

import (
	"github.com/zzir/agents-go/agents/session"
)

// GroupKind says what a group holds.
type GroupKind int

const (
	// GroupSystem is instructions and other system-role content.
	GroupSystem GroupKind = iota
	// GroupUser is one user message.
	GroupUser
	// GroupAssistantText is assistant prose with no tool calls.
	GroupAssistantText
	// GroupToolCall is a tool call, its output, and any reasoning that led to
	// it — the pairing that must never be split.
	GroupToolCall
	// GroupSummary is a compaction checkpoint.
	GroupSummary
	// GroupOther is everything that is not conversation: annotations, terminal
	// records, custom entries.
	GroupOther
)

func (k GroupKind) String() string {
	switch k {
	case GroupSystem:
		return "system"
	case GroupUser:
		return "user"
	case GroupAssistantText:
		return "assistant"
	case GroupToolCall:
		return "tool_call"
	case GroupSummary:
		return "summary"
	case GroupOther:
		return "other"
	}
	return "unknown"
}

// Group is a run of entries that must be kept or dropped together.
type Group struct {
	Kind    GroupKind
	Entries []session.Entry
	// TurnIndex is the conversation turn this group belongs to, counted from
	// user messages. Nil for system content and for entries outside the
	// conversation, which belong to no turn.
	TurnIndex *int
	// Tokens is the group's estimated size.
	Tokens int

	// Excluded marks a group a strategy has removed from the context. The
	// group itself is NOT deleted: exclusion is a view, so a strategy can be
	// re-run, undone, or explained, and the stored history stays intact.
	Excluded bool
	// settled marks an exclusion that a later model call has already priced
	// in: once new entries arrive, any usage they carry measured the view
	// WITHOUT this group, so ContextTokens must stop subtracting it. Fresh
	// exclusions (this pass, no call since) are the ones the newest usage
	// still counts.
	settled bool
	// ExcludeReason names the strategy that excluded it, for tracing and for
	// telling a user why their history shrank.
	ExcludeReason string
	// Replacement, when set, is what the group projects to instead of its
	// entries — a folded tool-result summary, for example.
	Replacement []session.Entry
}

// classify reports an entry's role in the conversation.
func classify(e session.Entry) (kind GroupKind, isCall, isOutput, isReasoning bool) {
	switch e.Kind {
	case session.EntryKindCompaction:
		return GroupSummary, false, false, false
	case session.EntryKindItem:
	default:
		return GroupOther, false, false, false
	}

	p := session.ProbeItem(e.Item)
	switch p.Type {
	case "function_call":
		return GroupToolCall, true, false, false
	case "function_call_output":
		return GroupToolCall, false, true, false
	case "reasoning":
		return GroupAssistantText, false, false, true
	}
	switch p.Role {
	case "system", "developer":
		return GroupSystem, false, false, false
	case "user":
		return GroupUser, false, false, false
	case "assistant":
		return GroupAssistantText, false, false, false
	}
	// A message with no role and no known type: an item kind this build does
	// not model. Treat it as assistant content so it stays in context rather
	// than being silently reclassified as non-conversation.
	if p.Type == "message" {
		return GroupAssistantText, false, false, false
	}
	return GroupOther, false, false, false
}
