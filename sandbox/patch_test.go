package sandbox

import "testing"

func TestParsePatchUpdate(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: f\n a\n-b\n+B\n c\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(edits) != 1 || edits[0].op != opUpdate || edits[0].path != "f" {
		t.Fatalf("edits = %+v", edits)
	}
	got, err := applyHunks("a\nb\nc\n", edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "a\nB\nc\n" {
		t.Fatalf("got %q, want %q", got, "a\nB\nc\n")
	}
}

func TestParsePatchAddDeleteMove(t *testing.T) {
	add, _ := parsePatch("*** Begin Patch\n*** Add File: new.txt\n+hello\n+world\n*** End Patch\n")
	if len(add) != 1 || add[0].op != opAdd || add[0].addBody != "hello\nworld" {
		t.Fatalf("add = %+v", add)
	}
	del, _ := parsePatch("*** Begin Patch\n*** Delete File: gone.txt\n*** End Patch\n")
	if len(del) != 1 || del[0].op != opDelete || del[0].path != "gone.txt" {
		t.Fatalf("del = %+v", del)
	}
	mv, err := parsePatch("*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n a\n-b\n+B\n*** End Patch\n")
	if err != nil || len(mv) != 1 || mv[0].movePath != "new.go" {
		t.Fatalf("move = %+v err %v", mv, err)
	}
}

func TestApplyHunksMultiAndAnchor(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: f\n one\n-two\n+TWO\n@@\n three\n-four\n+FOUR\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := applyHunks("one\ntwo\nthree\nfour\n", edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "one\nTWO\nthree\nFOUR\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyHunksContextNotFound(t *testing.T) {
	edits, _ := parsePatch("*** Begin Patch\n*** Update File: f\n gone\n-x\n+y\n*** End Patch\n")
	if _, err := applyHunks("totally\ndifferent\n", edits[0].hunks); err == nil {
		t.Fatal("expected context-not-found error")
	}
}

func TestParsePatchErrors(t *testing.T) {
	// Pure insertion without an anchor is rejected (can't be located).
	if _, err := parsePatch("*** Begin Patch\n*** Update File: f\n+justadd\n*** End Patch\n"); err == nil {
		t.Fatal("pure-insertion without anchor should fail")
	}
	// Missing header.
	if _, err := parsePatch("*** Update File: f\n a\n*** End Patch\n"); err == nil {
		t.Fatal("missing Begin Patch should fail")
	}
	// Empty patch body.
	if _, err := parsePatch("*** Begin Patch\n*** End Patch\n"); err == nil {
		t.Fatal("no file sections should fail")
	}
}

// A model may emit git-style hunk headers ("@@ -a,b +c,d @@"). The line-number
// range is ignored (we locate by context); the patch must still apply.
func TestApplyGitStyleHunkHeader(t *testing.T) {
	src := "\"\"\"\ncompute primes 0-1000\n\"\"\"\n"
	patch := "*** Begin Patch\n*** Update File: f\n@@ -1,3 +1,3 @@\n \"\"\"\n-compute primes 0-1000\n+compute primes 2000-2500\n \"\"\"\n*** End Patch\n"
	edits, err := parsePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := applyHunks(src, edits[0].hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got != "\"\"\"\ncompute primes 2000-2500\n\"\"\"\n" {
		t.Fatalf("got %q", got)
	}
}

// A git header may carry a section heading after the range; it becomes the
// anchor, and a plain "@@ heading" (Codex form) still works.
func TestParseHunkAnchorForms(t *testing.T) {
	cases := map[string]string{
		"@@ -1,3 +1,3 @@":              "",
		"@@ -22,4 +22,4 @@ def foo():": "def foo():",
		"@@ def bar()":                 "def bar()",
		"@@":                           "",
	}
	for header, want := range cases {
		if got := parseHunkAnchor(header); got != want {
			t.Errorf("parseHunkAnchor(%q) = %q, want %q", header, got, want)
		}
	}
}

// Delete + Add of one path is the full-rewrite idiom: the delete removes the
// file, the add recreates it, and both apply correctly. A guard that refused
// every repeated path blocked it — and suggested merging two sections that
// cannot be merged.
func TestPatchAllowsDeleteThenAddOfOnePath(t *testing.T) {
	edits, err := parsePatch("*** Begin Patch\n" +
		"*** Delete File: a.txt\n" +
		"*** Add File: a.txt\n" +
		"+rewritten\n" +
		"*** End Patch\n")
	if err != nil {
		t.Fatalf("the full-rewrite idiom was refused: %v", err)
	}
	if len(edits) != 2 {
		t.Fatalf("parsed %d sections, want 2", len(edits))
	}
}

// Every other repeat stays refused: each section reads the file as it was
// BEFORE the patch, so the outcome would depend on the order they are applied.
func TestPatchRefusesOrderDependentRepeats(t *testing.T) {
	cases := map[string]string{
		"two updates": "*** Update File: a.txt\n@@\n-old\n+new\n" +
			"*** Update File: a.txt\n@@\n-other\n+thing\n",
		"delete then update": "*** Delete File: a.txt\n" +
			"*** Update File: a.txt\n@@\n-old\n+new\n",
		"add then update": "*** Add File: a.txt\n+body\n" +
			"*** Update File: a.txt\n@@\n-body\n+other\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePatch("*** Begin Patch\n" + body + "*** End Patch\n"); err == nil {
				t.Error("an order-dependent patch was accepted; one section's changes would be silently discarded")
			}
		})
	}
}
