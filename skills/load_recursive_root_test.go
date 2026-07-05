package skills

import (
	"path/filepath"
	"testing"
)

func TestLoadRecursiveMissingRootErrors(t *testing.T) {
	_, err := LoadRecursive(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("LoadRecursive on a missing root should error, not return zero skills")
	}
}

func TestLoadRecursiveEmptyRootIsNotAnError(t *testing.T) {
	skills, err := LoadRecursive(t.TempDir())
	if err != nil {
		t.Fatalf("empty (but existing) root should not error: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("expected no skills, got %d", len(skills))
	}
}
