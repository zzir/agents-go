package agents

import (
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

const (
	defaultHistoryStartMarker = "<CONVERSATION HISTORY>"
	defaultHistoryEndMarker   = "</CONVERSATION HISTORY>"

	// historySummaryLeadIn is the fixed first line of every summary message
	// produced by summaryMessage. flattenNestedHistory only expands assistant
	// messages that begin with it, so an ordinary message that merely quotes
	// the markers is never mistaken for a summary.
	historySummaryLeadIn = "For context, here is the conversation so far between the user and the previous agent:"
	// historyEmptyPlaceholder is the summary body emitted for an empty
	// transcript; flattening recognizes it and yields an empty transcript.
	historyEmptyPlaceholder = "(no previous turns recorded)"
)

// HandoffHistoryMapper folds a flattened transcript into the input items the
// target agent receives after a handoff. The default emits one assistant message
// summarizing the transcript; supply your own to, for example, call an LLM for a
// real summary.
type HandoffHistoryMapper func(transcript []InputItem) []InputItem

// NestHistoryOptions configures NestHandoffHistory.
type NestHistoryOptions struct {
	// Mapper folds the transcript into the target agent's input. Defaults to a
	// mapper that emits one assistant message wrapping the transcript in the
	// fixed "<CONVERSATION HISTORY>" markers. Flattening only recognizes that
	// default summary shape; a custom Mapper still works, but its summaries are
	// treated as opaque messages by later handoffs (no flattening).
	Mapper HandoffHistoryMapper
}

// NestHandoffHistory returns a Handoff InputFilter that summarizes the prior
// conversation into a compact form for the next agent.
//
// Before summarizing, it flattens any summary produced by an earlier handoff
// back into its underlying transcript, so a chain of handoffs yields one flat
// summary rather than a summary-of-summaries. The transcript is serialized as
// one JSON item per line (via session.MarshalInputItem), which round-trips through
// session.UnmarshalInputItem when later flattened.
//
// Like every InputFilter it only changes what the target agent sees; it does not
// alter what is saved to the session.
func NestHandoffHistory(opts NestHistoryOptions) func(HandoffInputData) HandoffInputData {
	mapper := opts.Mapper
	if mapper == nil {
		mapper = func(transcript []InputItem) []InputItem {
			return []InputItem{summaryMessage(transcript)}
		}
	}
	return func(data HandoffInputData) HandoffInputData {
		transcript := flattenNestedHistory(data.InputHistory)
		return HandoffInputData{InputHistory: mapper(transcript)}
	}
}

// summaryMessage renders the transcript as a numbered, marker-wrapped assistant
// message. Each transcript item is one compact JSON line.
func summaryMessage(transcript []InputItem) InputItem {
	lines := make([]string, 0, len(transcript)+4)
	lines = append(lines,
		historySummaryLeadIn,
		defaultHistoryStartMarker,
	)
	if len(transcript) == 0 {
		lines = append(lines, historyEmptyPlaceholder)
	} else {
		for i, item := range transcript {
			data, err := session.MarshalInputItem(item)
			if err != nil {
				continue
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+string(data))
		}
	}
	lines = append(lines, defaultHistoryEndMarker)
	return responses.ResponseInputItemParamOfMessage(strings.Join(lines, "\n"), responses.EasyInputMessageRoleAssistant)
}

// flattenNestedHistory expands any prior nested-history summary message back into
// its underlying transcript, leaving all other items untouched.
func flattenNestedHistory(items []InputItem) []InputItem {
	out := make([]InputItem, 0, len(items))
	for _, item := range items {
		if nested, ok := extractNestedTranscript(item); ok {
			out = append(out, nested...)
			continue
		}
		out = append(out, item)
	}
	return out
}

// extractNestedTranscript parses an item that is a marker-wrapped summary back
// into its transcript items. ok reports whether the item was such a summary.
//
// Only the SDK's own summary shape is expandable: an assistant message whose
// text starts with the fixed lead-in line and contains the markers. Anything
// else — in particular a user message quoting the markers — is left untouched;
// expanding arbitrary marker-bearing text would let conversation content
// inject or silently delete history.
func extractNestedTranscript(item InputItem) ([]InputItem, bool) {
	m := item.OfMessage
	if m == nil || m.Role != responses.EasyInputMessageRoleAssistant {
		return nil, false
	}
	content := m.Content.OfString.Or("")
	if !strings.HasPrefix(content, historySummaryLeadIn) {
		return nil, false
	}
	si := strings.Index(content, defaultHistoryStartMarker)
	ei := strings.LastIndex(content, defaultHistoryEndMarker)
	if si == -1 || ei == -1 || ei <= si {
		return nil, false
	}
	body := content[si+len(defaultHistoryStartMarker) : ei]
	var parsed []InputItem
	sawUnparsable := false
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == historyEmptyPlaceholder {
			continue
		}
		line = stripLineNumber(line)
		it, err := session.UnmarshalInputItem([]byte(line))
		if err != nil {
			sawUnparsable = true
			continue
		}
		parsed = append(parsed, it)
	}
	if sawUnparsable {
		// One loss policy, not two: a transcript with ANY line this build
		// cannot decode is kept verbatim, exactly like one with no decodable
		// lines. Flattening the readable subset silently deleted the other
		// lines from the target agent's view — three items gone with no
		// diagnostic, while a fully unreadable summary survived intact.
		return nil, false
	}
	return parsed, true
}

// stripLineNumber removes a leading "N. " prefix produced by summaryMessage.
func stripLineNumber(line string) string {
	if dot := strings.Index(line, ". "); dot > 0 {
		if _, err := strconv.Atoi(line[:dot]); err == nil {
			return line[dot+2:]
		}
	}
	return line
}
