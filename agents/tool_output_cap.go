package agents

import (
	"fmt"
	"unicode/utf8"
)

// capToolOutput bounds the text a tool result sends to the model — spec §2.7b.
// Text parts are elided, other parts kept; a value that is neither is capped
// as the text it would be sent as.
func capToolOutput(out any, limit int) any {
	if limit < 0 {
		return out
	}
	if limit == 0 {
		limit = DefaultToolOutputLimit
	}
	switch v := out.(type) {
	case string:
		return elideText(v, limit)
	case ToolOutputText:
		return ToolOutputText{Text: elideText(v.Text, limit)}
	case []ToolOutputContent:
		var capped []ToolOutputContent
		for i, part := range v {
			t, ok := part.(ToolOutputText)
			if !ok || len(t.Text) <= limit {
				continue
			}
			if capped == nil {
				capped = append([]ToolOutputContent(nil), v...)
			}
			capped[i] = ToolOutputText{Text: elideText(t.Text, limit)}
		}
		if capped == nil {
			return v
		}
		return capped
	case nil:
		return out
	default:
		if s := stringifyToolOutput(v); len(s) > limit {
			return elideText(s, limit)
		}
		return out
	}
}

// elideText keeps the head (60%) and the tail (40%) of s within limit bytes,
// cut on rune boundaries, around a marker naming what was left out.
func elideText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	headEnd := limit * 3 / 5
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - (limit - headEnd)
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	if tailStart < headEnd {
		tailStart = headEnd
	}
	return fmt.Sprintf("%s\n[omitted %d bytes]\n%s", s[:headEnd], tailStart-headEnd, s[tailStart:])
}
