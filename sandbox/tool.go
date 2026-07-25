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
	// Name is the tool name exposed to the model. Defaults to "exec_command".
	Name string
	// Description tells the model what the tool does. A sensible default is
	// used when empty.
	Description string
	// Timeout is the default timeout per execution; zero means
	// sandbox.DefaultTimeout. The model can override this per call via
	// the timeout_seconds argument (capped at MaxTimeout).
	Timeout time.Duration
	// MaxTimeout caps the per-command timeout the model may request. Zero
	// means 10 minutes.
	MaxTimeout time.Duration
	// MaxOutputBytes truncates each of stdout and stderr sent to the model (so
	// a tool result carries at most about twice this many output bytes).
	// Defaults to 8192. The cut never splits a multi-byte UTF-8 sequence.
	MaxOutputBytes int
	// NeedsApprovalFunc, when set, is forwarded to the tool as its per-call
	// approval gate: given the command in argsJSON and the model-assigned callID
	// it decides whether this execution must be approved first. nil = never gate.
	// The sandbox package attaches no policy of its own — the caller supplies the
	// decision.
	NeedsApprovalFunc func(ctx context.Context, rc *agents.RunContext, argsJSON string, callID string) (bool, error)
}

const defaultMaxTimeout = 10 * time.Minute

func (c CodeToolConfig) withDefaults() CodeToolConfig {
	if c.Name == "" {
		c.Name = "exec_command"
	}
	if c.Description == "" {
		c.Description = "Execute a shell command in a sandboxed environment and return its stdout, stderr and exit code. " +
			"The command is run via bash -c. " +
			"Use timeout_seconds to override the default timeout for long-running commands. " +
			"Use workdir to run the command in a specific directory (relative paths are resolved from /workspace)."
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8192
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = defaultMaxTimeout
	}
	return c
}

type codeToolArgs struct {
	Cmd            string `json:"cmd"             jsonschema:"the shell command to execute (passed to bash -c)"`
	TimeoutSeconds int    `json:"timeout_seconds" jsonschema:"execution timeout in seconds; 0 uses the default"`
	Workdir        string `json:"workdir"         jsonschema:"working directory for the command; empty uses the sandbox default"`
}

// CodeTool wraps a Sandbox as a function tool. The model supplies a shell
// command which is executed via bash -c; stdout, stderr and exit code are
// returned as text. Execution errors (non-zero exit, timeout) are returned
// to the model as output so it can correct itself.
func CodeTool(sb Sandbox, cfg CodeToolConfig) agents.Tool {
	cfg = cfg.withDefaults()
	schema, schemaErr := agents.SchemaFor[codeToolArgs](true)
	if schemaErr != nil {
		return &agents.FunctionTool{
			Name:             cfg.Name,
			Description:      cfg.Description,
			ParamsJSONSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false, "required": []any{}},
			Strict:           true,
			OnInvoke: func(context.Context, *agents.ToolContext, string) (any, error) {
				return nil, agents.Classify(agents.CodeUserError, fmt.Errorf("code tool %q: schema generation failed: %w", cfg.Name, schemaErr))
			},
		}
	}
	return &agents.FunctionTool{
		Name:              cfg.Name,
		Description:       cfg.Description,
		ParamsJSONSchema:  schema,
		Strict:            true,
		NeedsApprovalFunc: cfg.NeedsApprovalFunc,
		OnInvoke: func(ctx context.Context, _ *agents.ToolContext, argsJSON string) (any, error) {
			var args codeToolArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, agents.Classify(agents.CodeModelBehavior, fmt.Errorf("code tool %q: invalid arguments: %w", cfg.Name, err))
			}

			timeout := cfg.Timeout
			if args.TimeoutSeconds > 0 {
				requested := time.Duration(args.TimeoutSeconds) * time.Second
				if requested > cfg.MaxTimeout {
					requested = cfg.MaxTimeout
				}
				timeout = requested
			}

			cmd := args.Cmd
			if args.Workdir != "" {
				cmd = "cd " + shellQuote(args.Workdir) + " && " + cmd
			}

			res, err := sb.Exec(ctx, ExecRequest{
				Cmd:     []string{"bash", "-c", cmd},
				Timeout: timeout,
			})
			if err != nil {
				return nil, agents.Classify(agents.CodeSandboxExec, fmt.Errorf("code tool %q: %w", cfg.Name, err))
			}
			return formatResult(res, cfg.MaxOutputBytes), nil
		},
	}
}

// shellQuote wraps s in single quotes for POSIX shell, escaping embedded
// single quotes.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// formatResult renders an ExecResult as the string sent back to the model.
func formatResult(res *ExecResult, limit int) string {
	var b strings.Builder
	if res.TimedOut {
		b.WriteString("[timed out]\n")
	}
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if res.Stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(truncateWithInfo(res.Stdout, limit))
		b.WriteString("\n")
	}
	if res.Stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(truncateWithInfo(res.Stderr, limit))
		b.WriteString("\n")
	}
	return b.String()
}

// truncateWithInfo cuts s to at most max bytes, backing up to a rune boundary
// so a multi-byte UTF-8 sequence is never split. When truncated, the total
// byte count is reported so the model knows how much was cut.
func truncateWithInfo(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	if cut == 0 {
		return fmt.Sprintf("…[truncated, showing 0 of %d bytes]", len(s))
	}
	return fmt.Sprintf("%s\n…[truncated, showing %d of %d bytes]", s[:cut], cut, len(s))
}
