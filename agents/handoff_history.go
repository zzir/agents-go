package agents

import (
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

const (
	defaultHistoryStartMarker = "<CONVERSATION HISTORY>"
	defaultHistoryEndMarker   = "</CONVERSATION HISTORY>"
)

// HandoffHistoryMapper folds a flattened transcript into the input items the
// target agent receives after a handoff. The default emits one assistant message
// summarizing the transcript; supply your own to, for example, call an LLM for a
// real summary.
type HandoffHistoryMapper func(transcript []TResponseInputItem) []TResponseInputItem

// NestHistoryOptions configures NestHandoffHistory.
type NestHistoryOptions struct {
	// Mapper folds the transcript into the target agent's input. Defaults to a
	// mapper that emits one assistant message wrapping the transcript in the
	// start/end markers.
	Mapper HandoffHistoryMapper
	// StartMarker and EndMarker wrap the summary body so a subsequent handoff can
	// flatten it back into a transcript instead of nesting summaries. They default
	// to "<CONVERSATION HISTORY>" and "</CONVERSATION HISTORY>". A custom Mapper
	// that wants flattening to work must wrap its output in these same markers.
	StartMarker string
	EndMarker   string
}

// NestHandoffHistory returns a Handoff InputFilter that summarizes the prior
// conversation into a compact form for the next agent, mirroring the Python
// SDK's nest_handoff_history.
//
// Before summarizing, it flattens any summary produced by an earlier handoff
// back into its underlying transcript, so a chain of handoffs yields one flat
// summary rather than a summary-of-summaries. The transcript is serialized as
// one JSON item per line (via MarshalInputItem), which round-trips through
// UnmarshalInputItem when later flattened.
//
// Like every InputFilter it only changes what the target agent sees; it does not
// alter what is saved to the session.
func NestHandoffHistory(opts NestHistoryOptions) func(HandoffInputData) HandoffInputData {
	start := opts.StartMarker
	if start == "" {
		start = defaultHistoryStartMarker
	}
	end := opts.EndMarker
	if end == "" {
		end = defaultHistoryEndMarker
	}
	mapper := opts.Mapper
	if mapper == nil {
		mapper = func(transcript []TResponseInputItem) []TResponseInputItem {
			return []TResponseInputItem{summaryMessage(transcript, start, end)}
		}
	}
	return func(data HandoffInputData) HandoffInputData {
		transcript := flattenNestedHistory(data.InputHistory, start, end)
		return HandoffInputData{InputHistory: mapper(transcript)}
	}
}

// DefaultHandoffHistoryMapper folds a transcript into a single assistant message
// using the default markers. It is the mapper NestHandoffHistory uses when none
// is supplied, exported so callers can reuse or wrap it.
func DefaultHandoffHistoryMapper(transcript []TResponseInputItem) []TResponseInputItem {
	return []TResponseInputItem{summaryMessage(transcript, defaultHistoryStartMarker, defaultHistoryEndMarker)}
}

// summaryMessage renders the transcript as a numbered, marker-wrapped assistant
// message. Each transcript item is one compact JSON line.
func summaryMessage(transcript []TResponseInputItem, start, end string) TResponseInputItem {
	lines := make([]string, 0, len(transcript)+4)
	lines = append(lines,
		"For context, here is the conversation so far between the user and the previous agent:",
		start,
	)
	if len(transcript) == 0 {
		lines = append(lines, "(no previous turns recorded)")
	} else {
		for i, item := range transcript {
			data, err := MarshalInputItem(item)
			if err != nil {
				continue
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+string(data))
		}
	}
	lines = append(lines, end)
	return responses.ResponseInputItemParamOfMessage(strings.Join(lines, "\n"), responses.EasyInputMessageRoleAssistant)
}

// flattenNestedHistory expands any prior nested-history summary message back into
// its underlying transcript, leaving all other items untouched.
func flattenNestedHistory(items []TResponseInputItem, start, end string) []TResponseInputItem {
	out := make([]TResponseInputItem, 0, len(items))
	for _, item := range items {
		if nested, ok := extractNestedTranscript(item, start, end); ok {
			out = append(out, nested...)
			continue
		}
		out = append(out, item)
	}
	return out
}

// extractNestedTranscript parses an item that is a marker-wrapped summary back
// into its transcript items. ok reports whether the item was such a summary.
func extractNestedTranscript(item TResponseInputItem, start, end string) ([]TResponseInputItem, bool) {
	content := inputItemText(item)
	if content == "" {
		return nil, false
	}
	si := strings.Index(content, start)
	ei := strings.LastIndex(content, end)
	if si == -1 || ei == -1 || ei <= si {
		return nil, false
	}
	body := content[si+len(start) : ei]
	var parsed []TResponseInputItem
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = stripLineNumber(line)
		it, err := UnmarshalInputItem([]byte(line))
		if err != nil {
			continue
		}
		parsed = append(parsed, it)
	}
	return parsed, true
}

// inputItemText returns the plain-text content of an easy (role) input message,
// or "" for any other item shape.
func inputItemText(item TResponseInputItem) string {
	if m := item.OfMessage; m != nil {
		return m.Content.OfString.Or("")
	}
	return ""
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
