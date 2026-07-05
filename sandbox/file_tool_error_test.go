package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/zzir/agents-go/agents"
)

// invokeTool runs a file tool and returns its string output.
func invokeTool(t *testing.T, tool agents.Tool, args string) string {
	t.Helper()
	ft := tool.(*agents.FunctionTool)
	out, err := ft.OnInvoke(context.Background(), &agents.ToolContext{}, args)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.(string)
	return s
}

func TestFileToolErrors_NoHostPathLeak(t *testing.T) {
	dir := t.TempDir()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir, MaxReadFileBytes: 8})
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		tool     agents.Tool
		args     string
		wantKind string
	}{
		{"read missing", ReadFileTool(sb, FileToolConfig{}), `{"path":"missing.txt"}`, "not found"},
		{"read over limit", ReadFileTool(sb, FileToolConfig{}), `{"path":"big.bin"}`, "file exceeds read limit"},
		{"list missing dir", ListFilesTool(sb, FileToolConfig{}), `{"path":"no/such/dir"}`, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := invokeTool(t, tc.tool, tc.args)
			if !strings.HasPrefix(out, "error: ") {
				t.Errorf("output = %q, want an error message", out)
			}
			if !strings.Contains(out, tc.wantKind) {
				t.Errorf("output = %q, want error kind %q", out, tc.wantKind)
			}
			// The host absolute working directory must never reach the model.
			if strings.Contains(out, dir) {
				t.Errorf("output leaks the host working directory: %q", out)
			}
		})
	}
}

func TestFileToolErrors_IncludeRequestPath(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	out := invokeTool(t, ReadFileTool(sb, FileToolConfig{}), `{"path":"sub/wanted.txt"}`)
	if !strings.Contains(out, "sub/wanted.txt") {
		t.Errorf("output = %q, want it to echo the requested path", out)
	}
}

func TestFileToolErrors_NoWorkDir(t *testing.T) {
	sb := NewLocal() // no WorkDir: every file op fails with ErrNoWorkDir
	for name, tc := range map[string]struct {
		tool agents.Tool
		args string
	}{
		"read":  {ReadFileTool(sb, FileToolConfig{}), `{"path":"a.txt"}`},
		"write": {WriteFileTool(sb, FileToolConfig{}), `{"path":"a.txt","content":"x"}`},
		"list":  {ListFilesTool(sb, FileToolConfig{}), `{"path":""}`},
	} {
		t.Run(name, func(t *testing.T) {
			out := invokeTool(t, tc.tool, tc.args)
			if !strings.Contains(out, "no persistent working directory") {
				t.Errorf("output = %q, want the no-workdir kind", out)
			}
		})
	}
}

func TestFileToolError_UnknownErrorIsGeneric(t *testing.T) {
	// An arbitrary backend error (which might embed anything) is reduced to a
	// generic kind.
	got := fileToolError("read", "x.txt", os.ErrClosed)
	want := "error: read x.txt: operation failed"
	if got != want {
		t.Errorf("fileToolError = %q, want %q", got, want)
	}
}
