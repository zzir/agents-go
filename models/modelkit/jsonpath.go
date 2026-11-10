package modelkit

import "strings"

// escapeJSONPath escapes sjson path metacharacters so WithJSONSet (openai-go
// and anthropic-sdk-go share the machinery) treats k as one literal top-level
// key; a leading ':' is sjson's force-string-key marker and is escaped too.
func escapeJSONPath(k string) string {
	var b strings.Builder
	for i, r := range k {
		switch r {
		case '.', '*', '?', '|', '#', '@', '\\':
			b.WriteByte('\\')
		case ':':
			// Only a leading ':' is special to sjson.
			if i == 0 {
				b.WriteByte('\\')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
