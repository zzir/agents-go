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

	// historySummaryLeadIn is the fixed first line of every summaryMessage;
	// flattenNestedHistory expands only messages that begin with it.
	historySummaryLeadIn = "For context, here is the conversation so far between the user and the previous agent:"
	// historyEmptyPlaceholder is the summary body emitted for an empty
	// transcript; flattening recognizes it and yields an empty transcript.
	historyEmptyPlaceholder = "(no previous turns recorded)"
)

// HandoffHistoryMapper folds a flattened transcript into the input items the
// target agent receives after a handoff. The default emits one assistant
// message summarizing the transcript; supply your own to call an LLM instead.
type HandoffHistoryMapper func(transcript []InputItem) []InputItem

// NestHistoryOptions configures NestHandoffHistory.
type NestHistoryOptions struct {
	// Mapper folds the transcript into the target agent's input; the default
	// emits one assistant message in the fixed markers. Only that shape is
	// flattened by later handoffs; a custom Mapper's summaries stay opaque.
	Mapper HandoffHistoryMapper
}

// NestHandoffHistory returns a Handoff InputFilter that summarizes the prior
// conversation for the next agent. A summary from an earlier handoff is
// flattened back into its transcript first, so a chain yields one flat
// summary; items are serialized one JSON line each via session.MarshalInputItem.
// Like every InputFilter it changes only what the target sees, not the session.
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

// extractNestedTranscript parses a marker-wrapped summary back into its items.
// Only the SDK's own summary shape expands; arbitrary marker text could inject history.
func extractNestedTranscript(item InputItem) ([]InputItem, bool) {
	m := item.OfMessage
	if m == nil || m.Role != responses.EasyInputMessageRoleAssistant {
		return nil, false
	}
	content := m.Content.OfString.Or("")
	if !strings.HasPrefix(content, historySummaryLeadIn) {
		return nil, false
	}
	_, rest, foundStart := strings.Cut(content, defaultHistoryStartMarker)
	body, _, foundEnd := strings.CutLast(rest, defaultHistoryEndMarker)
	if !foundStart || !foundEnd {
		return nil, false
	}
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
		// One loss policy: a transcript with ANY undecodable line is kept verbatim,
		// rather than flattening the readable subset and silently dropping the rest.
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
