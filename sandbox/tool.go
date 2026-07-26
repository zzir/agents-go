package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
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
			OnInvoke: func(context.Context, *agents.ToolContext, string) (agents.ToolResult, error) {
				return agents.ToolResult{}, agents.Classify(agents.CodeUserError, fmt.Errorf("code tool %q: schema generation failed: %w", cfg.Name, schemaErr))
			},
		}
	}
	return &agents.FunctionTool{
		Name:              cfg.Name,
		Description:       cfg.Description,
		ParamsJSONSchema:  schema,
		Strict:            true,
		NeedsApprovalFunc: cfg.NeedsApprovalFunc,
		OnInvoke: func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (agents.ToolResult, error) {
			var args codeToolArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return agents.ToolResult{}, agents.Classify(agents.CodeModelBehavior, fmt.Errorf("code tool %q: invalid arguments: %w", cfg.Name, err))
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

			// Instrumented here rather than in each backend: this is the one
			// place every sandbox — local, Docker, SSH — is reached through.
			span, ctx := tracing.StartSpanFrom(ctx, "sandbox.exec", tracing.SpanTypeSandbox,
				map[string]any{"tool": cfg.Name, "timeout_ms": timeout.Milliseconds()})
			defer span.Finish()

			req := ExecRequest{Cmd: []string{"bash", "-c", cmd}, Timeout: timeout}
			var res *ExecResult
			var err error
			if streamer, ok := sb.(ExecStreamer); ok && tc != nil {
				// A command producing output for two minutes is unwatchable
				// otherwise; the model still gets only the final result.
				//
				// ExecStream hands output to the writers instead of capturing
				// it, so the capped buffers below are what the result is built
				// from — streaming must not cost the model its output.
				maxOut := req.EffectiveMaxOutputBytes()
				outBuf := &CappedBuffer{Max: maxOut}
				errBuf := &CappedBuffer{Max: maxOut}
				w := &progressWriter{tc: tc, cmd: cmd}
				res, err = streamer.ExecStream(ctx,
					req, io.MultiWriter(outBuf, w), io.MultiWriter(errBuf, w))
				w.flush()
				if res != nil {
					res.Stdout, res.Stderr = outBuf.String(), errBuf.String()
				}
			} else {
				res, err = sb.Exec(ctx, req)
			}
			if err != nil {
				span.SetError(err.Error(), nil)
				return agents.ToolResult{}, agents.Classify(agents.CodeSandboxExec, fmt.Errorf("code tool %q: %w", cfg.Name, err))
			}
			span.Set("exit_code", res.ExitCode)
			span.Set("timed_out", res.TimedOut)
			// The exit code and streams belong in Details, not only folded into
			// the text the model reads: a UI showing a command should not have
			// to parse "exit_code: 1" back out of a formatted blob.
			return agents.TextResult(formatResult(res, cfg.MaxOutputBytes)).
				WithDisplay("terminal").
				WithDetails(map[string]any{
					"command":   cmd,
					"exit_code": res.ExitCode,
					"timed_out": res.TimedOut,
				}), nil
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

// truncateWithInfo cuts s to at most limit bytes, keeping the **head and the
// tail** and eliding the middle.
//
// Head-only truncation loses exactly the part that matters most: a build or
// test command prints its progress first and its failure summary last, so
// cutting the tail hands the model the least useful half of the output. The
// split is 60/40 in favor of the head, which keeps the command's context while
// still reaching the verdict.
//
// Rune boundaries are respected on both sides so a multi-byte UTF-8 sequence is
// never split, and the elision line reports how much was dropped.
func truncateWithInfo(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}

	headLimit := limit * 3 / 5
	tailLimit := limit - headLimit

	head := s[:backToRuneStart(s, headLimit)]
	tail := s[forwardToRuneStart(s, len(s)-tailLimit):]

	dropped := len(s) - len(head) - len(tail)
	if dropped <= 0 {
		return s
	}
	if head == "" && tail == "" {
		return fmt.Sprintf("…[truncated, showing 0 of %d bytes]", len(s))
	}
	return fmt.Sprintf("%s\n…[%d of %d bytes elided]\n%s", head, dropped, len(s), tail)
}

// backToRuneStart moves i back to the nearest rune boundary at or before it.
func backToRuneStart(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for back := 0; back < utf8.UTFMax && i > 0 && !utf8.RuneStart(s[i]); back++ {
		i--
	}
	return i
}

// forwardToRuneStart moves i forward to the nearest rune boundary at or after
// it, so the tail never begins mid-sequence.
func forwardToRuneStart(s string, i int) int {
	if i <= 0 {
		return 0
	}
	for fwd := 0; fwd < utf8.UTFMax && i < len(s) && !utf8.RuneStart(s[i]); fwd++ {
		i++
	}
	return i
}

// progressWriter turns a sandbox's output stream into tool-progress events.
//
// It batches: a command emitting a line at a time would otherwise produce an
// event per line, and a consumer redrawing per event would spend more time
// rendering than the command spends running. Output is coalesced until a write
// completes a line, which is the granularity a terminal view wants anyway.
type progressWriter struct {
	tc  *agents.ToolContext
	cmd string
	buf []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if i := bytes.LastIndexByte(w.buf, '\n'); i >= 0 {
		w.send(string(w.buf[:i+1]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// flush emits whatever did not end in a newline — the last line of output,
// or a prompt a command left hanging.
func (w *progressWriter) flush() {
	if len(w.buf) > 0 {
		w.send(string(w.buf))
		w.buf = nil
	}
}

func (w *progressWriter) send(text string) {
	w.tc.Emit(agents.TextResult(text).
		WithDisplay("terminal").
		WithDetails(map[string]any{"command": w.cmd, "partial": true}))
}
