package sandbox

import "testing"

// a pure-insertion hunk anchored on the last line of a file that has no
// trailing newline must not glue the inserted text onto that last line — a
// newline is added first so the insertion starts on its own line.
func TestApplyHunksInsertNoTrailingNewline(t *testing.T) {
	src := "line1\nline2" // no trailing newline; "line2" is the final line
	patch := "*** Begin Patch\n*** Update File: f\n@@ line2\n+inserted\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := applyHunks(src, edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "line1\nline2\ninserted\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Companion to the above: with a trailing newline present the insertion already
// worked, and must still land on its own line after it.
func TestApplyHunksInsertWithTrailingNewline(t *testing.T) {
	src := "line1\nline2\n"
	patch := "*** Begin Patch\n*** Update File: f\n@@ line2\n+inserted\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := applyHunks(src, edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "line1\nline2\ninserted\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// a completely empty line ("" — a " " context line whose trailing space a
// model stripped) inside a hunk must be kept as an empty context line, not
// truncate the hunk into two and silently drop the empty line.
func TestParseHunkEmptyContextLineNotDropped(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: f\n a\n\n-b\n+B\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	if n := len(edits[0].hunks); n != 1 {
		t.Fatalf("hunks = %d, want 1 (the empty line must not split the hunk)", n)
	}
	h := edits[0].hunks[0]
	if h.oldBlock != "a\n\nb" {
		t.Errorf("oldBlock = %q, want %q", h.oldBlock, "a\n\nb")
	}
	if h.newBlock != "a\n\nB" {
		t.Errorf("newBlock = %q, want %q", h.newBlock, "a\n\nB")
	}
	got, err := applyHunks("a\n\nb\n", edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "a\n\nB\n" {
		t.Errorf("apply = %q, want %q", got, "a\n\nB\n")
	}
}

// The fix must not swallow a blank line that genuinely separates two hunks:
// when the next non-blank line begins a new @@ hunk, the blank stays a
// separator.
func TestParseHunkBlankSeparatorBetweenHunks(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: f\n one\n-two\n+TWO\n\n@@\n three\n-four\n+FOUR\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := len(edits[0].hunks); n != 2 {
		t.Fatalf("hunks = %d, want 2 (blank between hunks is a separator)", n)
	}
	got, err := applyHunks("one\ntwo\nthree\nfour\n", edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "one\nTWO\nthree\nFOUR\n" {
		t.Fatalf("got %q", got)
	}
}

// Nor a trailing blank line right before the End Patch footer: it is a
// separator, not an empty context line appended to the last hunk.
func TestParseHunkTrailingBlankBeforeEnd(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: f\n a\n-b\n+B\n\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h := edits[0].hunks[0]; h.oldBlock != "a\nb" {
		t.Errorf("oldBlock = %q, want %q (trailing blank must not be absorbed)", h.oldBlock, "a\nb")
	}
}
