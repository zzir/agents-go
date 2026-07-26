package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
)

// EnvironmentProbe describes what a sandbox looks like from the inside.
type EnvironmentProbe struct {
	// OS and Arch are the uname values.
	OS, Arch string
	// Shell is the login shell, when it could be determined.
	Shell string
	// WorkDir is the working directory commands start in.
	WorkDir string
	// Tools maps a probed command to its reported version, for the ones that
	// were found. A tool that is absent is simply not in the map.
	Tools map[string]string
}

// String renders the probe as instructions text.
func (e EnvironmentProbe) String() string {
	var b strings.Builder
	b.WriteString("Execution environment:\n")
	if e.OS != "" {
		fmt.Fprintf(&b, "- os: %s", e.OS)
		if e.Arch != "" {
			fmt.Fprintf(&b, " (%s)", e.Arch)
		}
		b.WriteString("\n")
	}
	if e.Shell != "" {
		fmt.Fprintf(&b, "- shell: %s\n", e.Shell)
	}
	if e.WorkDir != "" {
		fmt.Fprintf(&b, "- working directory: %s\n", e.WorkDir)
	}
	if len(e.Tools) > 0 {
		b.WriteString("- available tools: ")
		first := true
		for _, name := range probeOrder {
			v, ok := e.Tools[name]
			if !ok {
				continue
			}
			if !first {
				b.WriteString(", ")
			}
			first = false
			b.WriteString(name)
			if v != "" {
				fmt.Fprintf(&b, " %s", v)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// probeOrder fixes the reporting order, so identical environments produce
// identical instructions — a prompt that reorders between runs defeats prompt
// caching for no benefit.
var probeOrder = []string{"git", "go", "python3", "node", "rg", "jq", "curl", "make"}

// ProbeEnvironment asks the sandbox what it is.
//
// Telling a model what it is working with, once, up front is cheaper than
// letting it find out: without this it opens with `uname -a`, then `which go`,
// then discovers halfway through that the tool it planned around is missing.
// Each of those is a turn.
//
// It is ONE command, not one per fact: a round trip into a container or over
// SSH costs far more than the shell does, and ten probes would be ten of them.
func ProbeEnvironment(ctx context.Context, sb Sandbox) (EnvironmentProbe, error) {
	script := `echo "os=$(uname -s 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "shell=$SHELL"
echo "cwd=$(pwd)"
for t in ` + strings.Join(probeOrder, " ") + `; do
  command -v "$t" >/dev/null 2>&1 && echo "tool=$t $($t --version 2>/dev/null | head -n1)"
done`

	res, err := sb.Exec(ctx, ExecRequest{
		Cmd:     []string{"bash", "-c", script},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		return EnvironmentProbe{}, fmt.Errorf("probing sandbox environment: %w", err)
	}
	return parseProbe(res.Stdout), nil
}

func parseProbe(out string) EnvironmentProbe {
	e := EnvironmentProbe{Tools: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "os":
			e.OS = val
		case "arch":
			e.Arch = val
		case "shell":
			e.Shell = val
		case "cwd":
			e.WorkDir = val
		case "tool":
			name, version, _ := strings.Cut(val, " ")
			if name != "" {
				e.Tools[name] = strings.TrimSpace(version)
			}
		}
	}
	if len(e.Tools) == 0 {
		e.Tools = nil
	}
	return e
}

// EnvironmentInstructions probes the sandbox once and returns Instructions that
// append the result to whatever the agent already says.
//
// It probes ONCE, at construction, rather than per run: the environment does
// not change between turns, and re-probing would spend a round trip per run to
// re-learn it. A probe that fails contributes nothing rather than failing the
// agent — an agent that cannot describe its environment still works, one that
// refuses to start does not.
func EnvironmentInstructions(ctx context.Context, sb Sandbox, base agents.Instructions) agents.Instructions {
	probe, err := ProbeEnvironment(ctx, sb)
	if err != nil {
		return base
	}
	text := probe.String()
	if strings.TrimSpace(text) == "" {
		return base
	}
	if base == nil {
		base = agents.StaticInstructions("")
	}
	return agents.WrapInstructions(base, "", "\n\n"+text)
}
