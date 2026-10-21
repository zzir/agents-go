package bridge

import (
	"slices"
	"testing"
)

// The names plan mode admits are each server's read_only_tools behind that
// server's prefix: a tool the config does not name stays refused, whatever
// the server says about it (invariant 89).
func TestPlanReadOnlyNamesComeFromServerConfig(t *testing.T) {
	got := planReadOnlyNames([]attachedMCP{
		{name: "docs", readOnly: []string{"search", "", "fetch"}},
		{name: "db"},
		{name: "git", readOnly: []string{"log"}},
	})
	want := []string{"docs__search", "docs__fetch", "git__log"}
	if !slices.Equal(got, want) {
		t.Fatalf("planReadOnlyNames = %v, want %v", got, want)
	}
	if got := planReadOnlyNames(nil); len(got) != 0 {
		t.Fatalf("no servers list %v", got)
	}
}
