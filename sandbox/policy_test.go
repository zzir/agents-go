package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestPolicy_AllowRestricts(t *testing.T) {
	p := Policy{Allow: []string{`^git `, `^ls\b`}}
	if err := p.Check("git status"); err != nil {
		t.Errorf("an allowed command was refused: %v", err)
	}
	if err := p.Check("rm -rf /"); err == nil {
		t.Error("a command matching no allow pattern was permitted")
	}
}

// A deny always wins, so "allow git, deny git push" means what it looks like.
func TestPolicy_DenyBeatsAllow(t *testing.T) {
	p := Policy{Allow: []string{`^git `}, Deny: []string{`git push`}}
	if err := p.Check("git status"); err != nil {
		t.Errorf("git status was refused: %v", err)
	}
	err := p.Check("git push origin main")
	if err == nil {
		t.Fatal("git push was permitted")
	}
	// The reason names the rule, so a model can explain rather than try
	// variations until something works.
	if !strings.Contains(err.Error(), "git push") {
		t.Errorf("error = %v, want it to name the pattern", err)
	}
}

func TestPolicy_ZeroValueAllowsEverything(t *testing.T) {
	var p Policy
	if err := p.Check("anything at all"); err != nil {
		t.Errorf("the zero policy refused a command: %v", err)
	}
	if !p.Empty() {
		t.Error("the zero policy is not empty")
	}
}

// A policy that silently matched nothing would be worse than no policy: it
// looks like protection.
func TestPolicy_BadPatternRefusesEverything(t *testing.T) {
	p := Policy{Deny: []string{`([`}}
	if err := p.Compile(); err == nil {
		t.Fatal("a malformed pattern compiled")
	}
	if err := p.Check("ls"); err == nil {
		t.Error("an uncompilable policy fell open")
	}
}

// The refusal reaches the model as a result, so it can choose something else —
// not as an error it might retry verbatim.
func TestPolicy_RefusalIsAToolResult(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	defer sb.Close()
	tool := CodeTool(sb, CodeToolConfig{Policy: Policy{Deny: []string{`rm -rf`}}})
	inv, ok := tool, tool.OnInvoke != nil
	if !ok {
		t.Fatal("not invokable")
	}
	res, err2 := inv.OnInvoke(context.Background(),
		&agents.ToolContext{RunContext: agents.NewRunContext(nil)},
		`{"cmd":"rm -rf /tmp/x","timeout_seconds":0,"workdir":""}`)
	if err2 != nil {
		t.Fatalf("the refusal was returned as an error: %v", err2)
	}
	out, _ := res.ModelOutput().(string)
	if !strings.Contains(out, "refused by policy") {
		t.Errorf("output = %q, want the refusal explained", out)
	}
	if res.Details["refused"] != true {
		t.Errorf("details = %v, want the refusal flagged for the UI", res.Details)
	}
}

// The policy runs before the sandbox, so a refused command never executes.
func TestPolicy_RefusedCommandNeverRuns(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	defer sb.Close()

	tool := CodeTool(sb, CodeToolConfig{Policy: Policy{Deny: []string{`touch`}}})
	inv := tool
	if _, err := inv.OnInvoke(context.Background(),
		&agents.ToolContext{RunContext: agents.NewRunContext(nil)},
		`{"cmd":"touch marker","timeout_seconds":0,"workdir":""}`); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile(context.Background(), "marker"); err == nil {
		t.Error("a refused command executed anyway")
	}
}
