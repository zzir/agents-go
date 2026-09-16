package handler

import (
	"strings"
	"testing"
)

// A fork's name keeps its suffix and stays inside the cap a rename is held to.
func TestBranchNameKeepsTheNameCap(t *testing.T) {
	if got := branchName("Sorting a list", "fork"); got != "Sorting a list (fork)" {
		t.Fatalf("branchName = %q", got)
	}
	if got := branchName("Sorting a list (fork)", "fork"); got != "Sorting a list (fork 2)" {
		t.Fatalf("branchName of a fork = %q", got)
	}
	long := strings.Repeat("x", maxNameLen)
	got := branchName(long, "regen")
	if n := len([]rune(got)); n > maxNameLen {
		t.Fatalf("branchName of a max-length name is %d runes, over the cap %d", n, maxNameLen)
	}
	if !strings.HasSuffix(got, "… (regen)") {
		t.Fatalf("branchName of a max-length name = %q, want a clipped base and the suffix", got[len(got)-20:])
	}
}
