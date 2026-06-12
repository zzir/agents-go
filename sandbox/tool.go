package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	agents "github.com/zzir/agents-go/agents"
)

// CodeToolConfig configures CodeTool.
type CodeToolConfig struct {
	// Name is the tool name exposed to the model. Defaults to "run_code".
	Name string
	// Description tells the model what the tool runs. A sensible default is used
	// when empty.
	Description string
	// Filename is where the model's code is written in the working directory.
	// Defaults to "main.py".
	Filename string
	// RunCmd is the command that executes the file. Defaults to
	// ["python", "main.py"].
	RunCmd []string
	// Timeout bounds each execution; zero means sandbox.DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes truncates each of stdout and stderr sent to the model (so
	// a tool result carries at most about twice this many output bytes).
	// Defaults to 8192. The cut never splits a multi-byte UTF-8 sequence.
	MaxOutputBytes int
}

func (c CodeToolConfig) withDefaults() CodeToolConfig {
	if c.Name == "" {
		c.Name = "run_code"
	}
	if c.Description == "" {
		c.Description = "Execute code in a sandboxed environment and return its stdout, stderr and exit code."
	}
	if c.Filename == "" {
		c.Filename = "main.py"
	}
	if len(c.RunCmd) == 0 {
		c.RunCmd = []string{"python", "main.py"}
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8192
	}
	return c
}

type codeToolArgs struct {
	Code string `json:"code" jsonschema:"the code to execute"`
}

// CodeTool wraps a Sandbox as a function tool. The model supplies code, which is
// written to cfg.Filename and run via cfg.RunCmd; the result is returned as text.
// Execution errors (non-zero exit, timeout) are returned to the model as output
// so it can correct itself.
func CodeTool(sb Sandbox, cfg CodeToolConfig) agents.Tool {
	cfg = cfg.withDefaults()
	schema, err := agents.SchemaFor[codeToolArgs](true)
	if err != nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false, "required": []any{}}
	}
	return &agents.FunctionTool{
		Name:             cfg.Name,
		Description:      cfg.Description,
		ParamsJSONSchema: schema,
		Strict:           true,
		// FailureErrorFunction is intentionally nil: an error from OnInvoke means
		// the sandbox infrastructure failed (daemon down, missing image, bad
		// arguments) — something the model cannot fix — so it aborts the run with
		// a clear error instead of being fed back as a vague "tool error". Code
		// that runs but exits non-zero is returned as normal output (below) so
		// the model can correct it.
		OnInvoke: func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (any, error) {
			var args codeToolArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, fmt.Errorf("code tool %q: invalid arguments: %w", cfg.Name, err)
			}
			res, err := sb.Exec(ctx, ExecRequest{
				Cmd:     cfg.RunCmd,
				Files:   map[string]string{cfg.Filename: args.Code},
				Timeout: cfg.Timeout,
			})
			if err != nil {
				return nil, fmt.Errorf("code tool %q: %w", cfg.Name, err)
			}
			return formatResult(res, cfg.MaxOutputBytes), nil
		},
	}
}

// formatResult renders an ExecResult as the string sent back to the model.
func formatResult(res *ExecResult, max int) string {
	var b strings.Builder
	if res.TimedOut {
		b.WriteString("[timed out]\n")
	}
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if res.Stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(truncate(res.Stdout, max))
		b.WriteString("\n")
	}
	if res.Stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(truncate(res.Stderr, max))
		b.WriteString("\n")
	}
	return b.String()
}

// truncate cuts s to at most max bytes, backing up to a rune boundary so a
// multi-byte UTF-8 sequence is never split.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	// s[cut] is the first excluded byte; if it is a continuation byte the rune
	// straddles the cut, so back up (bounded: invalid UTF-8 is cut as-is).
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	return s[:cut] + "\n…[truncated]"
}
