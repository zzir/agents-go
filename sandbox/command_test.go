package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/zzir/agents-go/agents"
)

// noopBackground is a host that accepts the hand-off and answers with a handle.
func noopBackground(_ context.Context, _ *agents.ToolContext, cmd *Command) (agents.ToolResult, error) {
	return agents.TextResult("started " + cmd.CallID), nil
}

// The schema advertises session_id and background only when the tool offers
// them; the required list follows, so a strict backend never demands either.
func TestCodeTool_SchemaOffersOnlyWhatIsEnabled(t *testing.T) {
	cases := []struct {
		sessions   bool
		background bool
		want       []string
	}{
		{false, false, []string{"cmd", "timeout_seconds", "workdir"}},
		{true, false, []string{"cmd", "session_id", "timeout_seconds", "workdir"}},
		{false, true, []string{"background", "cmd", "timeout_seconds", "workdir"}},
		{true, true, []string{"background", "cmd", "session_id", "timeout_seconds", "workdir"}},
	}
	for _, tc := range cases {
		cfg := CodeToolConfig{Sessions: tc.sessions, SpillDir: "x"}
		if tc.background {
			cfg.Background = noopBackground
		}
		schema := CodeTool(NewLocal(), cfg).ParamsJSONSchema
		props, _ := schema["properties"].(map[string]any)
		got := slices.Sorted(func(yield func(string) bool) {
			for k := range props {
				if !yield(k) {
					return
				}
			}
		})
		if !slices.Equal(got, tc.want) {
			t.Errorf("sessions=%v background=%v: properties %v, want %v", tc.sessions, tc.background, got, tc.want)
		}
		raw, _ := json.Marshal(schema["required"])
		var req []string
		_ = json.Unmarshal(raw, &req)
		slices.Sort(req)
		if !slices.Equal(req, tc.want) {
			t.Errorf("sessions=%v background=%v: required %v, want %v", tc.sessions, tc.background, req, tc.want)
		}
	}
}

func TestCodeTool_BackgroundNeedsSpillDir(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Background without SpillDir was accepted")
		}
	}()
	CodeTool(NewLocal(), CodeToolConfig{Background: noopBackground})
}

// A background call is handed over after the policy and the decode, with the
// line as the foreground would run it and the timeout capped for the background.
func TestCodeTool_BackgroundHandsTheCommandToTheHost(t *testing.T) {
	var got *Command
	tool := CodeTool(NewLocal(), CodeToolConfig{
		SpillDir:             "spill",
		BackgroundMaxTimeout: time.Hour,
		Background: func(_ context.Context, tc *agents.ToolContext, cmd *Command) (agents.ToolResult, error) {
			got = cmd
			return agents.TextResult("started " + tc.ToolCallID).WithDetails(map[string]any{"task_id": "t1"}), nil
		},
	})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "call_9"},
		`{"cmd":"make","timeout_seconds":0,"workdir":"build","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.ModelOutput().(string); s != "started call_9" || out.Details["task_id"] != "t1" {
		t.Errorf("the host's result did not come back verbatim: %q %v", s, out.Details)
	}
	if got == nil || got.Line != "cd 'build' && make" || got.CallID != "call_9" || got.Timeout != time.Hour {
		t.Errorf("command = %+v", got)
	}
	// A requested timeout is capped at the background's.
	_, _ = tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c"},
		`{"cmd":"make","timeout_seconds":30,"workdir":"","background":true}`)
	if got.Timeout != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", got.Timeout)
	}
	_, _ = tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c"},
		`{"cmd":"make","timeout_seconds":7200,"workdir":"","background":true}`)
	if got.Timeout != time.Hour {
		t.Errorf("timeout = %s, want the hour cap", got.Timeout)
	}
}

// The policy and the approval gate see a background call like any other.
func TestCodeTool_BackgroundIsPolicyCheckedAndGated(t *testing.T) {
	handed := false
	var gated string
	tool := CodeTool(NewLocal(), CodeToolConfig{
		SpillDir: "spill",
		Policy:   Policy{Deny: []string{"rm -rf"}},
		Background: func(context.Context, *agents.ToolContext, *Command) (agents.ToolResult, error) {
			handed = true
			return agents.TextResult("started"), nil
		},
		NeedsApprovalFunc: func(_ context.Context, _ *agents.RunContext, argsJSON, _ string) (bool, error) {
			gated = argsJSON
			return true, nil
		},
	})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":"rm -rf /","timeout_seconds":0,"workdir":"","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if handed || out.Details["refused"] != true {
		t.Errorf("a vetoed command reached the host: handed=%v details=%v", handed, out.Details)
	}
	args := `{"cmd":"make","timeout_seconds":0,"workdir":"","background":true}`
	if need, _ := tool.NeedsApprovalFunc(context.Background(), nil, args, "c"); !need || gated != args {
		t.Errorf("gate: need=%v args=%q", need, gated)
	}
}

// background: true is refused as text where the host offers no background, and
// together with a session: neither is silently run in the foreground.
func TestCodeTool_BackgroundRefusals(t *testing.T) {
	off := CodeTool(NewLocal(), CodeToolConfig{})
	out, err := off.OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":"sleep 0","timeout_seconds":0,"workdir":"","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.ModelOutput().(string); !out.IsError || !strings.Contains(s, "not available") {
		t.Errorf("not refused: IsError=%v %q", out.IsError, s)
	}
	// The zero-value sentinels mean false, not a refusal.
	for _, lit := range []string{"false", "null", "0"} {
		out, err := off.OnInvoke(context.Background(), &agents.ToolContext{},
			`{"cmd":"echo fg","timeout_seconds":0,"workdir":"","background":`+lit+`}`)
		if err != nil || out.IsError {
			t.Errorf("background: %s: err=%v IsError=%v", lit, err, out.IsError)
		}
	}

	on := CodeTool(&fakeTerminalSandbox{Sandbox: NewLocal(), started: make(chan struct{}, 8)},
		CodeToolConfig{Sessions: true, SpillDir: "spill", Background: noopBackground})
	out, err = on.OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":"make","timeout_seconds":0,"workdir":"","session_id":"build","background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.ModelOutput().(string); !out.IsError || !strings.Contains(s, "session_id") {
		t.Errorf("not refused: IsError=%v %q", out.IsError, s)
	}
}

func TestLenientBool(t *testing.T) {
	for in, want := range map[string]bool{`true`: true, `1`: true, `false`: false, `0`: false, `null`: false} {
		var got lenientBool
		if err := json.Unmarshal([]byte(in), &got); err != nil || bool(got) != want {
			t.Errorf("Unmarshal(%s) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{`"true"`, `2`, `{}`, `[]`} {
		var got lenientBool
		if err := json.Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("Unmarshal(%s) = %v, want error", in, got)
		}
	}
}

func backgroundCommand(t *testing.T, dir, line string, timeout time.Duration) (*Command, *LocalSandbox) {
	t.Helper()
	var got *Command
	tool := CodeTool(NewLocalWithOptions(LocalOptions{WorkDir: dir}), CodeToolConfig{
		MaxOutputBytes: 1024, SpillDir: ".agents/exec",
		BackgroundMaxTimeout: timeout,
		Background: func(_ context.Context, _ *agents.ToolContext, cmd *Command) (agents.ToolResult, error) {
			got = cmd
			return agents.TextResult("started"), nil
		},
	})
	raw, _ := json.Marshal(map[string]any{"cmd": line, "timeout_seconds": 0, "workdir": "", "background": true})
	if _, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "call_x"}, string(raw)); err != nil {
		t.Fatal(err)
	}
	return got, NewLocalWithOptions(LocalOptions{WorkDir: dir})
}

// Run redirects both streams to files inside the sandbox and reports the
// verdict first: the exit code, the time, the bytes and where the output is.
func TestCommand_RunRedirectsAndReportsTheVerdict(t *testing.T) {
	dir := t.TempDir()
	cmd, sb := backgroundCommand(t, dir, "echo out; echo err >&2; exit 3", time.Minute)
	res, err := cmd.Run(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := res.ModelOutput().(string)
	first, rest, _ := strings.Cut(s, "\n")
	if !strings.HasPrefix(first, "exit 3 · ") || !strings.Contains(first, "· stdout 4 B · stderr 4 B · full output: .agents/exec/") {
		t.Errorf("verdict line = %q", first)
	}
	if !strings.HasPrefix(rest, "exit_code: 3\nstdout:\nout\n\nstderr:\nerr\n") {
		t.Errorf("body = %q", rest)
	}
	for _, k := range []string{"stdout_path", "stderr_path"} {
		p, _ := res.Details[k].(string)
		if got, err := os.ReadFile(filepath.Join(dir, p)); err != nil || string(got) != map[string]string{"stdout_path": "out\n", "stderr_path": "err\n"}[k] {
			t.Errorf("%s %q: %q %v", k, p, got, err)
		}
	}
	if res.Details["exit_code"] != 3 || res.Details["timed_out"] != false || res.Details["stdout_bytes"] != int64(4) {
		t.Errorf("details = %v", res.Details)
	}
	if res.IsError {
		t.Error("a non-zero exit is a result, not an error")
	}
}

// Output past the cap comes back head and tail, and the file is the whole of it.
func TestCommand_RunReadsBackHeadAndTail(t *testing.T) {
	dir := t.TempDir()
	cmd, sb := backgroundCommand(t, dir, "seq 1 20000", time.Minute)
	res, err := cmd.Run(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := res.ModelOutput().(string)
	if !strings.Contains(s, "stdout:\n1\n2\n") || !strings.HasSuffix(s, "\n19999\n20000\n\n") {
		t.Errorf("edges lost: %q … %q", s[:min(80, len(s))], s[max(0, len(s)-40):])
	}
	if !strings.Contains(s, "bytes elided — full output: .agents/exec/") || strings.Contains(s, "not captured") {
		t.Errorf("marker = %q", s[max(0, len(s)-300):])
	}
	p, _ := res.Details["stdout_path"].(string)
	if got, err := os.ReadFile(filepath.Join(dir, p)); err != nil || len(got) != int(res.Details["stdout_bytes"].(int64)) || !strings.HasSuffix(string(got), "20000\n") {
		t.Errorf("file: %d bytes, %v", len(got), err)
	}
}

// The line's own cd does not move the output files: they sit under the
// sandbox's default directory, where the marker says they are.
func TestCommand_RunFilesStayUnderTheDefaultDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	var got *Command
	tool := CodeTool(NewLocalWithOptions(LocalOptions{WorkDir: dir}), CodeToolConfig{
		SpillDir: ".agents/exec",
		Background: func(_ context.Context, _ *agents.ToolContext, cmd *Command) (agents.ToolResult, error) {
			got = cmd
			return agents.TextResult("started"), nil
		},
	})
	if _, err := tool.OnInvoke(context.Background(), &agents.ToolContext{ToolCallID: "c"},
		`{"cmd":"pwd","timeout_seconds":0,"workdir":"sub","background":true}`); err != nil {
		t.Fatal(err)
	}
	res, err := got.Run(context.Background(), NewLocalWithOptions(LocalOptions{WorkDir: dir}))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := res.Details["stdout_path"].(string)
	out, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil {
		t.Fatalf("file not under the default directory: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(out)), "/sub") {
		t.Errorf("the command did not run in its workdir: %q", out)
	}
}

func TestCommand_RunTimeoutIsAResult(t *testing.T) {
	cmd, sb := backgroundCommand(t, t.TempDir(), "echo before; sleep 5; echo after", 300*time.Millisecond)
	res, err := cmd.Run(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := res.ModelOutput().(string)
	if !strings.HasPrefix(s, "timed out after 0s · ") || !strings.Contains(s, "[timed out]\n") || !strings.Contains(s, "before") {
		t.Errorf("output = %q", s)
	}
	if res.Details["timed_out"] != true {
		t.Errorf("details = %v", res.Details)
	}
}

func TestCommand_RunCancelIsAnError(t *testing.T) {
	cmd, sb := backgroundCommand(t, t.TempDir(), "sleep 5", time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := cmd.Run(ctx, sb)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's ending", err)
	}
}

func TestFmtBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 512: "512 B", 1024: "1 KB", 18432: "18 KB", 2355: "2.3 KB", 3_000_000: "2.9 MB"} {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
