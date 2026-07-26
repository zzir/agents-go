package agents

import (
	"bytes"
	"context"
	"log/slog"
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
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
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
			Tools:        []Tool{tool},
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

// Most of what the SDK says is Debug; a caller usually wants that without
// turning Debug on for their whole application.
func TestLogging_LevelOverride(t *testing.T) {
	log, buf := capture(t)
	info := slog.LevelInfo
	if _, err := RunSync(context.Background(), simpleAgent(t, "hi"), "go", RunOptions{
		Log: LogConfig{Logger: log, Level: &info},
	}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "run started") {
		t.Errorf("info records were dropped:\n%s", out)
	}
	if strings.Contains(out, "calling model") {
		t.Errorf("debug records survived an Info level override:\n%s", out)
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
