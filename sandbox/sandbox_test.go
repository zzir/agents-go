package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agents "github.com/zzir/agents-go/agents"
)

func TestLocalSandbox_Exec(t *testing.T) {
	sb := NewLocal()
	defer sb.Close()

	res, err := sb.Exec(context.Background(), ExecRequest{
		Files: map[string]string{"hello.txt": "world"},
		Cmd:   []string{"cat", "hello.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "world" {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestLocalSandbox_NonZeroExit(t *testing.T) {
	sb := NewLocal()
	res, err := sb.Exec(context.Background(), ExecRequest{Cmd: []string{"sh", "-c", "echo oops >&2; exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestLocalSandbox_Timeout(t *testing.T) {
	sb := NewLocal()
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd:     []string{"sleep", "5"},
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut = true")
	}
}

func TestLocalSandbox_BackgroundChildDoesNotBlockSuccess(t *testing.T) {
	sb := NewLocal()
	start := time.Now()
	// The shell exits 0 immediately and only the backgrounded grandchild keeps
	// the stdout/stderr pipes open (the verified repro blocked Exec for the
	// full 30s). WaitDelay must unblock Wait, and the successful exit must not
	// be reported as a timeout.
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd:     []string{"sh", "-c", "sleep 30 & echo hi"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Exec took %v; the grandchild held the pipes open", elapsed)
	}
	if res.TimedOut {
		t.Error("successful exit must not be reported as TimedOut")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hi") {
		t.Errorf("stdout = %q, want it to contain %q", res.Stdout, "hi")
	}
}

func TestLocalSandbox_MinimalEnvByDefault(t *testing.T) {
	t.Setenv("SANDBOX_TEST_SECRET", "leaked-host-secret")
	sb := NewLocal()
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd: []string{"sh", "-c", `echo "secret=[$SANDBOX_TEST_SECRET] path=[$PATH] extra=[$EXTRA]"`},
		Env: map[string]string{"EXTRA": "added"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "leaked-host-secret") {
		t.Errorf("host env leaked into the sandbox: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "path=[]") {
		t.Errorf("PATH should be inherited from the host: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "extra=[added]") {
		t.Errorf("request Env not applied: %q", res.Stdout)
	}
}

func TestLocalSandbox_InheritHostEnvOptIn(t *testing.T) {
	t.Setenv("SANDBOX_TEST_SECRET", "host-value")
	sb := NewLocalWithOptions(LocalOptions{InheritHostEnv: true})
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd: []string{"sh", "-c", `echo "secret=[$SANDBOX_TEST_SECRET]"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "secret=[host-value]") {
		t.Errorf("InheritHostEnv should pass the host environment through: %q", res.Stdout)
	}
}

func TestLocalSandbox_OutputCapped(t *testing.T) {
	sb := NewLocal()
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd:            []string{"sh", "-c", "yes x | head -c 100000; echo BIGDONE >&2"},
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if len(res.Stdout) != 1024 {
		t.Errorf("stdout length = %d, want capped at 1024", len(res.Stdout))
	}
	if !strings.Contains(res.Stderr, "BIGDONE") {
		t.Errorf("stderr (under cap) lost: %q", res.Stderr)
	}
}

func TestTruncateWithInfo_UTF8Safe(t *testing.T) {
	s := strings.Repeat("界", 10)   // 3 bytes per rune = 30 bytes total
	got := truncateWithInfo(s, 10) // 10 is not a rune boundary
	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	// head+tail: the budget splits 60/40, so both ends survive.
	if !strings.HasPrefix(got, "界") || !strings.HasSuffix(got, "界") {
		t.Errorf("truncate did not keep both ends: %q", got)
	}
	if !strings.Contains(got, "of 30 bytes elided") {
		t.Errorf("missing byte count info: %q", got)
	}
	if r := truncateWithInfo(s, 30); r != s {
		t.Errorf("string at exactly max must not be truncated: %q", r)
	}
	got2 := truncateWithInfo("abcdef", 4)
	if !strings.HasPrefix(got2, "ab") || !strings.HasSuffix(got2, "ef") || !strings.Contains(got2, "of 6 bytes elided") {
		t.Errorf("ascii truncate = %q", got2)
	}
}

func TestCodeTool_WiringWithLocalSandbox(t *testing.T) {
	sb := NewLocal()
	tool := CodeTool(sb, CodeToolConfig{
		Name: "run_sh",
	})

	if tool.Name != "run_sh" {
		t.Errorf("name = %q", tool.Name)
	}
	if tool.ParamsJSONSchema["additionalProperties"] != false {
		t.Errorf("schema not strict: %v", tool.ParamsJSONSchema)
	}

	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"cmd":"echo hi from sandbox","timeout_seconds":0,"workdir":""}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "hi from sandbox") {
		t.Errorf("tool output missing stdout: %q", s)
	}
	if !strings.Contains(s, "exit_code: 0") {
		t.Errorf("tool output missing exit code: %q", s)
	}
}

func TestCodeTool_DefaultConfig(t *testing.T) {
	tool := CodeTool(NewLocal(), CodeToolConfig{})
	if tool.Name != "exec_command" {
		t.Errorf("default name = %q, want exec_command", tool.Name)
	}
}

func TestFileTools_ReadWriteList(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	tools := FileTools(sb, FileToolConfig{})

	if len(tools) != 3 {
		t.Fatalf("FileTools returned %d tools, want 3", len(tools))
	}

	ctx := context.Background()
	tc := &agents.ToolContext{}

	// write_file
	wt := tools[1]
	if wt.Name != "write_file" {
		t.Fatalf("tools[1].Name = %q, want write_file", wt.Name)
	}
	out, err := wt.OnInvoke(ctx, tc, `{"path":"test.txt","content":"hello file tools"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.ModelOutput().(string); !strings.Contains(s, "wrote") {
		t.Errorf("write_file output = %q", s)
	}

	// read_file
	rt := tools[0]
	if rt.Name != "read_file" {
		t.Fatalf("tools[0].Name = %q, want read_file", rt.Name)
	}
	out, err = rt.OnInvoke(ctx, tc, `{"path":"test.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.ModelOutput().(string); !strings.Contains(s, "hello file tools") {
		t.Errorf("read_file output = %q", s)
	}

	// list_files
	lt := tools[2]
	if lt.Name != "list_files" {
		t.Fatalf("tools[2].Name = %q, want list_files", lt.Name)
	}
	out, err = lt.OnInvoke(ctx, tc, `{"path":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.ModelOutput().(string); !strings.Contains(s, "test.txt") {
		t.Errorf("list_files output = %q", s)
	}
}

func TestReadFileTool_NotFound(t *testing.T) {
	sb := NewLocal()
	rt := ReadFileTool(sb, FileToolConfig{})
	out, err := rt.OnInvoke(context.Background(), &agents.ToolContext{}, `{"path":"/nonexistent/file"}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "error") {
		t.Errorf("expected error for missing file, got: %q", s)
	}
}
