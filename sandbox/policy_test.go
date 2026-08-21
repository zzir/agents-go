package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// The Policy doc offers these spellings as evidence that a pattern matches
// text, not intent. They are checked here so the claim keeps being true: a doc
// that names a walk-around which is in fact refused sells the policy as weaker
// than it is, and the next reader stops believing the rest of the paragraph.
func TestPolicy_DenyMatchesTextNotIntent(t *testing.T) {
	p := Policy{Deny: []string{`rm -rf`}}
	if err := p.Check("rm -rf /"); err == nil {
		t.Fatal("the literal spelling was permitted")
	}
	for _, cmd := range []string{
		"rm -fr /",
		"rm  -rf /",
		"eval $(echo cm0gLXJm | base64 -d)",
	} {
		if err := p.Check(cmd); err != nil {
			t.Errorf("Check(%q) = %v, want it permitted — the doc offers it as a walk-around", cmd, err)
		}
	}

	byPath := Policy{Deny: []string{`rm -rf /home/alice`}}
	if err := byPath.Check("rm -rf $HOME"); err != nil {
		t.Errorf("Check(rm -rf $HOME) = %v, want it permitted — the shell expands it later", err)
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

	// The tool compiles at construction, so the same must hold there: it
	// builds, and every command it is handed is refused as text.
	tool := CodeTool(NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()}), CodeToolConfig{Policy: p})
	res, err := tool.OnInvoke(context.Background(),
		&agents.ToolContext{RunContext: agents.NewRunContext(nil)},
		`{"cmd":"ls","timeout_seconds":0,"workdir":"","session_id":""}`)
	if err != nil {
		t.Fatalf("the refusal was returned as an error: %v", err)
	}
	if out, _ := res.ModelOutput().(string); !strings.Contains(out, "sandbox policy: deny") {
		t.Errorf("output = %q, want the malformed pattern reported", out)
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

// The runner executes a turn's tool calls in parallel, so one policy is read
// from several goroutines at once — and callers pass Policy around by value,
// so the copies must stand alone too. Worth having only under -race.
func TestPolicy_ConcurrentCheck(t *testing.T) {
	p := Policy{Allow: []string{`^git `, `^ls\b`}, Deny: []string{`git push`}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			shared, copied := &p, p
			for _, target := range []*Policy{shared, &copied} {
				if err := target.Check("git status"); err != nil {
					t.Errorf("an allowed command was refused: %v", err)
				}
				if err := target.Check("git push origin main"); err == nil {
					t.Error("git push was permitted")
				}
			}
		})
	}
	wg.Wait()
}

// CodeTool consults the policy from two closures — the approval gate and the
// invocation — and the runner can reach both at the same time.
func TestPolicy_ConcurrentThroughCodeTool(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	defer sb.Close()

	tool := CodeTool(sb, CodeToolConfig{
		Policy: Policy{Allow: []string{`^echo `}, Deny: []string{`rm -rf`}},
		NeedsApprovalFunc: func(context.Context, *agents.RunContext, string, string) (bool, error) {
			return true, nil
		},
	})

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		allowed := i%2 == 0
		cmd := "rm -rf /tmp/nonexistent"
		if allowed {
			cmd = "echo ok"
		}
		args := fmt.Sprintf(`{"cmd":%q,"timeout_seconds":0,"workdir":"","session_id":""}`, cmd)

		wg.Add(2)
		go func() {
			defer wg.Done()
			// A refused command is vetoed before the prompt, so the gate
			// answers "no approval needed" and OnInvoke refuses it as text.
			need, err := tool.NeedsApprovalFunc(ctx, agents.NewRunContext(nil), args, "call_1")
			if err != nil {
				t.Errorf("approval gate for %q: %v", cmd, err)
			}
			if need != allowed {
				t.Errorf("needs approval for %q = %v, want %v", cmd, need, allowed)
			}
		}()
		go func() {
			defer wg.Done()
			res, err := tool.OnInvoke(ctx,
				&agents.ToolContext{RunContext: agents.NewRunContext(nil)}, args)
			if err != nil {
				t.Errorf("invoking %q: %v", cmd, err)
			}
			out, _ := res.ModelOutput().(string)
			if refused := strings.Contains(out, "refused by policy"); refused == allowed {
				t.Errorf("output for %q = %q", cmd, out)
			}
		}()
	}
	wg.Wait()
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
