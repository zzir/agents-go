package agents

import (
	"sort"
)

// sortAnyStrings sorts a slice of any whose elements are strings, in place.
// Used to keep generated "required" lists deterministic (Go's json.Marshal
// already sorts map keys, so this keeps required aligned with properties).
func sortAnyStrings(s []any) {
	sort.Slice(s, func(i, j int) bool {
		si, _ := s[i].(string)
		sj, _ := s[j].(string)
		return si < sj
	})
}
