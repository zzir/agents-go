package modelkit

import "strings"

// EscapeJSONPath escapes sjson path metacharacters so the SDK option layers'
// WithJSONSet (openai-go and anthropic-sdk-go share the machinery) treats k as
// a single literal top-level key rather than a path expression — Python's
// extra_body semantics. A leading ':' is sjson's "force string key" marker —
// it is stripped from the key during path parsing — so it is escaped too,
// otherwise an ExtraBody key like ":k" would be silently renamed to "k".
func EscapeJSONPath(k string) string {
	var b strings.Builder
	for i, r := range k {
		switch r {
		case '.', '*', '?', '|', '#', '@', '\\':
			b.WriteByte('\\')
		case ':':
			// Only a leading ':' is special to sjson (force-string-key); a colon
			// anywhere else is an ordinary key character.
			if i == 0 {
				b.WriteByte('\\')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
