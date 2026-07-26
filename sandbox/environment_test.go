package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// Telling a model what it is working with, once, is cheaper than letting it
// find out with `uname -a`, then `which go`, then a failed plan.
func TestProbeEnvironment_ReportsTheSandbox(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	defer sb.Close()

	got, err := ProbeEnvironment(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	if got.OS == "" {
		t.Error("no OS reported")
	}
	if got.WorkDir == "" {
		t.Error("no working directory reported")
	}
	// go is running this test, so it is certainly present.
	if _, ok := got.Tools["go"]; !ok {
		t.Errorf("tools = %v, want go among them", got.Tools)
	}
	text := got.String()
	for _, want := range []string{"Execution environment:", "os:", "working directory:"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered probe missing %q:\n%s", want, text)
		}
	}
}

// Identical environments must render identically: a prompt that reorders
// between runs defeats prompt caching for no benefit.
func TestProbeEnvironment_RenderIsStable(t *testing.T) {
	p := EnvironmentProbe{
		OS: "Linux", Arch: "x86_64", WorkDir: "/w",
		Tools: map[string]string{"node": "v20", "git": "2.4", "go": "1.26"},
	}
	first := p.String()
	for range 20 {
		if p.String() != first {
			t.Fatalf("render is not stable:\n%s\n---\n%s", first, p.String())
		}
	}
	// And in the declared order, not map order.
	gi, ni := strings.Index(first, "git"), strings.Index(first, "node")
	if gi < 0 || ni < 0 || gi > ni {
		t.Errorf("tools are not in the fixed order:\n%s", first)
	}
}

func TestParseProbe_IgnoresNoise(t *testing.T) {
	got := parseProbe("os=Darwin\nnot a pair\narch=arm64\ntool=git git version 2.39.0\ntool=\n")
	if got.OS != "Darwin" || got.Arch != "arm64" {
		t.Errorf("probe = %+v", got)
	}
	if got.Tools["git"] != "git version 2.39.0" {
		t.Errorf("tools = %v", got.Tools)
	}
	if _, ok := got.Tools[""]; ok {
		t.Error("an empty tool name was recorded")
	}
}

// An agent that cannot describe its environment still works; one that refuses
// to start does not.
func TestEnvironmentInstructions_AppendsAndSurvivesFailure(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	defer sb.Close()

	base := agents.StaticInstructions("Be brief.")
	got := EnvironmentInstructions(ctx, sb, base)
	agent := &agents.Agent{Name: "a", Instructions: got}
	text, err := agent.GetSystemPrompt(ctx, agents.NewRunContext(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "Be brief.") {
		t.Errorf("the agent's own instructions were lost:\n%s", text)
	}
	if !strings.Contains(text, "Execution environment:") {
		t.Errorf("the probe was not appended:\n%s", text)
	}

	// A sandbox that cannot be probed leaves the instructions untouched.
	if got := EnvironmentInstructions(ctx, unprobableSandbox{}, base); got != base {
		t.Error("a failed probe changed the instructions")
	}
}

// unprobableSandbox refuses to execute anything.
type unprobableSandbox struct{ Sandbox }

func (unprobableSandbox) Exec(context.Context, ExecRequest) (*ExecResult, error) {
	return nil, context.DeadlineExceeded
}
