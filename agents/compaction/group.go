// Package compaction shrinks a session's history so a long conversation keeps
// fitting in a model's context window. The unit of work is a group, not an
// entry: a strategy only ever includes or excludes whole groups (decisions §5.56).
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
	// GroupToolCall is a tool call, its output, and any reasoning that led to it.
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
	// TurnIndex is the conversation turn this group belongs to, counted from user
	// messages; nil for system content and entries outside the conversation.
	TurnIndex *int
	// Tokens is the group's estimated size.
	Tokens int

	// Excluded marks a group a strategy removed from the context; nothing is
	// deleted (spec §2.5f).
	Excluded bool
	// settled marks an exclusion a later model call has priced in, so ContextTokens
	// stops subtracting it — see spec §2.5f.
	settled bool
	// ExcludeReason names the strategy that excluded it.
	ExcludeReason string
	// Replacement, when set, is what the group projects to instead of its entries.
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
	// A message with no role is an item kind this build does not model; keep it
	// in context.
	if p.Type == "message" {
		return GroupAssistantText, false, false, false
	}
	return GroupOther, false, false, false
}
