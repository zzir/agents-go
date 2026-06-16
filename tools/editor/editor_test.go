package editor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func toolMap(rootDir string) map[string]*agents.FunctionTool {
	m := map[string]*agents.FunctionTool{}
	for _, t := range NewTools(rootDir) {
		ft := t.(*agents.FunctionTool)
		m[ft.Name] = ft
	}
	return m
}

func call(t *testing.T, tool *agents.FunctionTool, argsJSON string) (string, error) {
	t.Helper()
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, argsJSON)
	if err != nil {
		return "", err
	}
	return out.(string), nil
}

func TestEditorLifecycle(t *testing.T) {
	root := t.TempDir()
	tools := toolMap(root)

	// create
	if _, err := call(t, tools["create_file"], `{"path":"notes.txt","content":"alpha\nbeta\ngamma"}`); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "notes.txt")); string(data) != "alpha\nbeta\ngamma" {
		t.Fatalf("created content = %q", data)
	}

	// create again -> fails (no clobber)
	if _, err := call(t, tools["create_file"], `{"path":"notes.txt","content":"x"}`); err == nil {
		t.Error("create over existing file should fail")
	}

	// view with line numbers
	out, err := call(t, tools["view_file"], `{"path":"notes.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1\talpha") || !strings.Contains(out, "3\tgamma") {
		t.Errorf("view output:\n%s", out)
	}

	// str_replace (unique)
	if _, err := call(t, tools["str_replace"], `{"path":"notes.txt","old_str":"beta","new_str":"BETA"}`); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "notes.txt")); string(data) != "alpha\nBETA\ngamma" {
		t.Fatalf("after replace = %q", data)
	}

	// insert after line 1
	if _, err := call(t, tools["insert_text"], `{"path":"notes.txt","line":1,"text":"inserted"}`); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "notes.txt")); string(data) != "alpha\ninserted\nBETA\ngamma" {
		t.Fatalf("after insert = %q", data)
	}
}

func TestStrReplaceUniqueness(t *testing.T) {
	root := t.TempDir()
	tools := toolMap(root)
	if _, err := call(t, tools["create_file"], `{"path":"f.txt","content":"x x x"}`); err != nil {
		t.Fatal(err)
	}
	// not found
	if _, err := call(t, tools["str_replace"], `{"path":"f.txt","old_str":"zzz","new_str":"y"}`); err == nil {
		t.Error("missing old_str should error")
	}
	// not unique
	if _, err := call(t, tools["str_replace"], `{"path":"f.txt","old_str":"x","new_str":"y"}`); err == nil {
		t.Error("non-unique old_str should error")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	tools := toolMap(root)
	if _, err := call(t, tools["view_file"], `{"path":"../../etc/passwd"}`); err == nil {
		t.Error("path traversal should be rejected")
	}
	if _, err := call(t, tools["create_file"], `{"path":"../escape.txt","content":"x"}`); err == nil {
		t.Error("create outside root should be rejected")
	}
}
