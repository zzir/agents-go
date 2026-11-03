package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agents "github.com/zzir/agents-go/agents"
)

// savedPath pulls the file name out of a marker's "full output: X]" or
// "saved to X, …" clause.
func savedPath(t *testing.T, out string) string {
	t.Helper()
	for _, lead := range []string{"full output: ", "saved to "} {
		if _, after, ok := strings.Cut(out, lead); ok {
			end := strings.IndexAny(after, ",]")
			if end < 0 {
				t.Fatalf("unterminated marker in %q", out)
			}
			return after[:end]
		}
	}
	t.Fatalf("no saved path in %q", out)
	return ""
}

// A truncated stream is saved whole under SpillDir, the marker names the file,
// and the directory keeps git out.
func TestCodeTool_SpillsTruncatedOutput(t *testing.T) {
	dir := t.TempDir()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir})
	tool := CodeTool(sb, CodeToolConfig{MaxOutputBytes: 1024, SpillDir: ".agents/exec"})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "call_abc/../x"},
		`{"cmd":"seq 1 2000","timeout_seconds":0,"workdir":""}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	p := savedPath(t, s)
	if !strings.HasPrefix(p, ".agents/exec/") || !strings.HasSuffix(p, "-call_abcx.stdout") {
		t.Errorf("saved path = %q", p)
	}
	got, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := 1; i <= 2000; i++ {
		want.WriteString(strconv.Itoa(i))
		want.WriteByte('\n')
	}
	if string(got) != want.String() {
		t.Errorf("spilled file is not the full output (%d bytes, want %d)", len(got), want.Len())
	}
	if ignore, err := os.ReadFile(filepath.Join(dir, ".agents/exec/.gitignore")); err != nil || string(ignore) != "*\n" {
		t.Errorf(".gitignore = %q, %v", ignore, err)
	}
	if len(s) > 1024+512 {
		t.Errorf("model output is %d bytes, want about MaxOutputBytes", len(s))
	}
}

// Past the capture cap the file has a hole too, and both the marker and the
// file say so instead of calling it the full output.
func TestCodeTool_SpillPastTheCaptureCapIsHonest(t *testing.T) {
	dir := t.TempDir()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir})
	tool := CodeTool(sb, CodeToolConfig{MaxOutputBytes: 1024, SpillDir: "out"})
	const n = 3_000_000
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c1"},
		fmt.Sprintf(`{"cmd":"yes | head -c %d","timeout_seconds":0,"workdir":""}`, n))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	hole := n - int(DefaultMaxOutputBytes)
	if want := fmt.Sprintf(", %d bytes of its middle not captured]", hole); !strings.Contains(s, want) {
		t.Errorf("marker lacks %q: %q", want, s[max(0, len(s)-300):])
	}
	if strings.Contains(s, "full output") {
		t.Errorf("a file with a hole was called the full output: %q", s[max(0, len(s)-300):])
	}
	got, err := os.ReadFile(filepath.Join(dir, savedPath(t, s)))
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("\n…[%d bytes not captured]\n", hole)
	if !strings.Contains(string(got), line) {
		t.Errorf("file lacks the hole line %q", line)
	}
	if want := int(DefaultMaxOutputBytes) + len(line); len(got) != want {
		t.Errorf("file is %d bytes, want %d (the capture plus the hole line)", len(got), want)
	}
}

// Under the limit nothing is written: no exec, no directory.
func TestCodeTool_NoSpillUnderTheLimit(t *testing.T) {
	dir := t.TempDir()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir})
	tool := CodeTool(sb, CodeToolConfig{MaxOutputBytes: 1024, SpillDir: ".agents/exec"})
	if _, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c"},
		`{"cmd":"echo fits","timeout_seconds":0,"workdir":""}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents")); !os.IsNotExist(err) {
		t.Errorf(".agents exists after an output that fit: %v", err)
	}
}

// failingFiles is a sandbox whose exec cannot take files: the spill's write fails.
type failingFiles struct{ *LocalSandbox }

func (f failingFiles) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if len(req.Files) > 0 {
		return nil, fmt.Errorf("disk full")
	}
	return f.LocalSandbox.Exec(ctx, req)
}

// A spill that fails costs the model nothing but the pointer: the result still
// comes back truncated, and never as an error.
func TestCodeTool_SpillFailureKeepsTheResult(t *testing.T) {
	sb := failingFiles{NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})}
	tool := CodeTool(sb, CodeToolConfig{MaxOutputBytes: 512, SpillDir: "spill"})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c"},
		`{"cmd":"seq 1 1000","timeout_seconds":0,"workdir":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError {
		t.Fatal("a failed spill became the tool's error")
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "bytes elided]") || strings.Contains(s, " — ") {
		t.Errorf("marker should be bare: %q", s[max(0, len(s)-200):])
	}
}

// A persistent shell's flood spills as one interleaved stream.
func TestCodeTool_SessionSpills(t *testing.T) {
	dir := t.TempDir()
	body := floodBody()
	sb := &fakeTerminalSandbox{
		Sandbox: NewLocalWithOptions(LocalOptions{WorkDir: dir}),
		started: make(chan struct{}, 8),
		respond: func(cmd string) (string, int) {
			if cmd == "flood" {
				return body, 0
			}
			return "", 0
		},
	}
	tool := CodeTool(sb, CodeToolConfig{Sessions: true, MaxOutputBytes: 300, SpillDir: ".agents/exec"})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c2"},
		`{"cmd":"flood","timeout_seconds":0,"workdir":"","session_id":"s"}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	p := savedPath(t, s)
	if !strings.HasSuffix(p, "-c2.out") {
		t.Errorf("saved path = %q", p)
	}
	got, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "first-line\n") || !strings.HasSuffix(string(got), "\nlast-line") {
		t.Errorf("file edges = %q … %q", got[:min(20, len(got))], got[max(0, len(got)-20):])
	}
}

func TestSpillScript(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "-mmin +10080 ", // 7 days
		-1:               "",              // never pruned
		30 * time.Second: "-mmin +1 ",     // never the files just written
		2 * time.Hour:    "-mmin +120 ",
	}
	for retention, want := range cases {
		got := spillScript("a b/exec", retention)
		if !strings.HasPrefix(got, "mkdir -p 'a b/exec'") || !strings.HasSuffix(got, "printf '*\\n' > 'a b/exec/.gitignore'") {
			t.Errorf("retention %s: %q", retention, got)
		}
		if want == "" {
			if strings.Contains(got, "find") {
				t.Errorf("retention %s should not prune: %q", retention, got)
			}
		} else if !strings.Contains(got, "find 'a b/exec' -type f ! -name .gitignore "+want+"-delete") {
			t.Errorf("retention %s: %q", retention, got)
		}
	}
}

func TestSpillID(t *testing.T) {
	if got := spillID("call_abc-1/../x y"); got != "call_abc-1xy" {
		t.Errorf("spillID = %q", got)
	}
	if got := spillID(strings.Repeat("z", 50)); len(got) != 40 {
		t.Errorf("len = %d, want 40", len(got))
	}
	if got := spillID("///"); len(got) != 16 {
		t.Errorf("no usable call id should yield 8 random bytes, got %q", got)
	}
}

func TestCodeTool_SpillDirMustBeRelative(t *testing.T) {
	for _, dir := range []string{"/tmp/spill", "../spill", ".", "a/../.."} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SpillDir %q was accepted", dir)
				}
			}()
			CodeTool(NewLocal(), CodeToolConfig{SpillDir: dir})
		}()
	}
}
