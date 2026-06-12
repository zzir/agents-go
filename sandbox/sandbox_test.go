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

func TestTruncate_UTF8Safe(t *testing.T) {
	s := strings.Repeat("界", 10) // 3 bytes per rune
	got := truncate(s, 10)       // 10 is not a rune boundary; 9 is
	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("界", 3)) {
		t.Errorf("truncate cut too much: %q", got)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("missing truncation marker: %q", got)
	}
	if r := truncate(s, 30); r != s {
		t.Errorf("string at exactly max must not be truncated: %q", r)
	}
	if r := truncate("abcdef", 4); r != "abcd\n…[truncated]" {
		t.Errorf("ascii truncate = %q", r)
	}
}

func TestCodeTool_WiringWithLocalSandbox(t *testing.T) {
	sb := NewLocal()
	tool := CodeTool(sb, CodeToolConfig{
		Name:     "run_sh",
		Filename: "script.sh",
		RunCmd:   []string{"sh", "script.sh"},
	}).(*agents.FunctionTool)

	if tool.Name != "run_sh" {
		t.Errorf("name = %q", tool.Name)
	}
	// The generated schema must be strict.
	if tool.ParamsJSONSchema["additionalProperties"] != false {
		t.Errorf("schema not strict: %v", tool.ParamsJSONSchema)
	}

	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"code":"echo hi from sandbox"}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.(string)
	if !strings.Contains(s, "hi from sandbox") {
		t.Errorf("tool output missing stdout: %q", s)
	}
	if !strings.Contains(s, "exit_code: 0") {
		t.Errorf("tool output missing exit code: %q", s)
	}
}

func TestCodeTool_DefaultConfig(t *testing.T) {
	tool := CodeTool(NewLocal(), CodeToolConfig{}).(*agents.FunctionTool)
	if tool.Name != "run_code" {
		t.Errorf("default name = %q, want run_code", tool.Name)
	}
}
