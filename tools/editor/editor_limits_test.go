package editor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// writeBigFile writes a size-byte file with recognizable needles at both ends
// and returns its exact content.
func writeBigFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	content := bytes.Repeat([]byte("x"), size)
	copy(content, "HEAD")
	copy(content[size-4:], "TAIL")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return content
}

// Edits of a file over maxFileBytes must be refused outright: a truncated read
// written back with O_TRUNC would silently destroy the tail of the file.
func TestOversizeEditRefused(t *testing.T) {
	root := t.TempDir()
	tools := toolMap(root)
	const size = maxFileBytes + 512
	orig := writeBigFile(t, filepath.Join(root, "big.txt"), size)

	if _, err := call(t, tools["str_replace"], `{"path":"big.txt","old_str":"HEAD","new_str":"head"}`); err == nil {
		t.Error("str_replace on an oversize file must be refused")
	} else if !strings.Contains(err.Error(), "edit limit") {
		t.Errorf("unexpected str_replace error: %v", err)
	}
	if _, err := call(t, tools["insert_text"], `{"path":"big.txt","line":0,"text":"x"}`); err == nil {
		t.Error("insert_text on an oversize file must be refused")
	}

	got, err := os.ReadFile(filepath.Join(root, "big.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatalf("refused edits must leave the file untouched (len %d -> %d)", len(orig), len(got))
	}
}

// Viewing a file over maxFileBytes succeeds but must carry an explicit
// truncation marker so the model never mistakes a partial file for the whole.
func TestViewOversizeFileHasTruncationMarker(t *testing.T) {
	root := t.TempDir()
	tools := toolMap(root)
	writeBigFile(t, filepath.Join(root, "big.txt"), maxFileBytes+512)

	out, err := call(t, tools["view_file"], `{"path":"big.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[... truncated: file is") {
		t.Errorf("view of oversize file must carry a truncation marker; tail = %q", out[len(out)-120:])
	}

	// A small file must not be marked truncated.
	if _, err := call(t, tools["create_file"], `{"path":"small.txt","content":"hi"}`); err != nil {
		t.Fatal(err)
	}
	small, err := call(t, tools["view_file"], `{"path":"small.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(small, "truncated") {
		t.Errorf("small file view must not be marked truncated:\n%s", small)
	}
}

// Two str_replace calls on the same file in the same turn run concurrently
// (the SDK executes tool calls in parallel); the shared mutex must serialize
// their read-modify-write cycles so both edits land.
func TestConcurrentStrReplaceKeepsBothEdits(t *testing.T) {
	for round := range 25 {
		root := t.TempDir()
		tools := toolMap(root)
		if _, err := call(t, tools["create_file"], `{"path":"f.txt","content":"alpha\nbeta\ngamma"}`); err != nil {
			t.Fatal(err)
		}

		calls := []string{
			`{"path":"f.txt","old_str":"alpha","new_str":"ALPHA"}`,
			`{"path":"f.txt","old_str":"gamma","new_str":"GAMMA"}`,
		}
		start := make(chan struct{})
		errs := make([]error, len(calls))
		var wg sync.WaitGroup
		for i, argsJSON := range calls {
			wg.Add(1)
			go func(i int, argsJSON string) {
				defer wg.Done()
				<-start
				_, errs[i] = tools["str_replace"].OnInvoke(context.Background(), &agents.ToolContext{}, argsJSON)
			}(i, argsJSON)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent str_replace %d failed: %v", round, i, err)
			}
		}

		data, err := os.ReadFile(filepath.Join(root, "f.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "ALPHA\nbeta\nGAMMA" {
			t.Fatalf("round %d: lost update, content = %q", round, data)
		}
	}
}
