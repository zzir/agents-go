package agents

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// capture builds a logger that records everything, so a test can assert both
// what was written and what was filtered out.
func capture(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// An SDK that logs to the process default the moment it is imported shows up
// uninvited in somebody's production output.
func TestLogging_SilentByDefault(t *testing.T) {
	log, buf := capture(t)
	old := slog.Default()
	slog.SetDefault(log)
	defer slog.SetDefault(old)

	if _, err := RunSync(context.Background(), simpleAgent(t, "hi"), "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("the SDK logged without being asked:\n%s", buf.String())
	}
}

func TestLogging_RecordsTheRunsShape(t *testing.T) {
	log, buf := capture(t)
	tool := NewFunctionTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{Log: LogConfig{Logger: log}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"run started", "turn started", "calling model", "model responded",
		"tool started", "tool finished",
		`component=run`, `component=tool`, `agent=a`, `tool=probe`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

// "Log what the SDK is doing" and "log what the user said" are different
// decisions; the one that leaks a conversation has to be made on purpose.
func TestLogging_SensitiveDataIsOptIn(t *testing.T) {
	run := func(sensitive bool) string {
		log, buf := capture(t)
		tool := NewFunctionTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
			return "ok", nil
		})
		agent := &Agent{
			Name:         "a",
			Instructions: StaticInstructions("SECRET-SYSTEM-PROMPT"),
			Tools:        []*FunctionTool{tool},
			ModelImpl: &fakeModel{responses: []*ModelResponse{
				modelResp(functionCallOutput(t, "probe", "c1", `{"q":"SECRET-ARGS"}`)),
				modelResp(messageOutput(t, "done")),
			}},
		}
		if _, err := RunSync(context.Background(), agent, "go", RunOptions{
			Log: LogConfig{Logger: log, SensitiveData: sensitive},
		}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	off := run(false)
	if strings.Contains(off, "SECRET-SYSTEM-PROMPT") || strings.Contains(off, "SECRET-ARGS") {
		t.Errorf("conversation content leaked without SensitiveData:\n%s", off)
	}
	if !strings.Contains(off, "calling model") {
		t.Error("filtering sensitive attributes dropped the record itself")
	}

	on := run(true)
	if !strings.Contains(on, "SECRET-SYSTEM-PROMPT") || !strings.Contains(on, "SECRET-ARGS") {
		t.Errorf("SensitiveData did not include the content:\n%s", on)
	}
	// And it is unwrapped, not a Go struct rendering.
	if strings.Contains(on, "sensitiveValue") {
		t.Errorf("a sensitive attribute rendered as its wrapper type:\n%s", on)
	}
}

// A Sensitive attribute handed to a plain slog.Logger — outside the SDK's
// opt-in filter — must render redacted, never the value. The only reveal path
// is LogConfig.SensitiveData.
func TestLogging_SensitiveRedactsOutsideTheFilter(t *testing.T) {
	log, buf := capture(t)
	log.LogAttrs(context.Background(), slog.LevelInfo, "own record", Sensitive("secret", "SECRET-VALUE"))
	out := buf.String()
	if strings.Contains(out, "SECRET-VALUE") {
		t.Errorf("a Sensitive attribute leaked through an unfiltered handler:\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("expected a redaction marker:\n%s", out)
	}
}

func TestLogging_NilLoggerIsANoOp(t *testing.T) {
	l := newRunLogger(LogConfig{})
	// Must not panic, and must report itself disabled so callers can skip work.
	l.Debug(context.Background(), "ignored", slog.String("k", "v"))
	if l.enabled(context.Background(), slog.LevelError) {
		t.Error("a nil-logger config reports itself enabled")
	}
	if c := l.component("x").with(slog.String("a", "b")); c == nil {
		t.Error("component/with on a disabled logger returned nil")
	}
}

// spec §2.11c: "the SDK never writes to slog.Default() on its own." A package
// -level slog call does exactly that, so it must not reappear — the failure is
// silent for whoever writes it and loud for whoever imports the library.
func TestNoPackageLevelSlogCalls(t *testing.T) {
	root := ".."
	bad := regexp.MustCompile(`slog\.(Default\(\)|Info|Warn|Error|Debug|InfoContext|WarnContext|ErrorContext|DebugContext)\(`)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "examples", "cmd", "agentstest", "dist":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d writes through the package-level slog: %s",
					path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A nested agent-as-tool run inherits the parent's log configuration, for the
// same reason it inherits the tracer: the part of the workflow hardest to see
// into must not be the part that goes silent.
func TestLogging_NestedAgentToolInheritsLogger(t *testing.T) {
	log, buf := capture(t)
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{Name: "specialist"})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"hi"}`)

	if _, err := RunSync(context.Background(), orch, "go", RunOptions{Log: LogConfig{Logger: log}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "agent=specialist") {
		t.Errorf("nested run left no log records; the parent logger was not inherited:\n%s", buf.String())
	}
}
