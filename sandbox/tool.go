package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
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
	// Timeout is the default per-execution timeout; zero means DefaultTimeout.
	// The model overrides it per call via timeout_seconds, capped at MaxTimeout.
	Timeout time.Duration
	// MaxTimeout caps the per-command timeout the model may request. Zero
	// means 10 minutes.
	MaxTimeout time.Duration
	// MaxOutputBytes truncates each of stdout and stderr sent to the model, on
	// a rune boundary. Defaults to 8192.
	MaxOutputBytes int
	// SpillDir, when set, is where a stream the model's view truncates is saved
	// whole: a relative path under the sandbox's working directory, made on
	// first use (spec §2.7v). Empty keeps the output in the result alone.
	SpillDir string
	// SpillRetention is how long a spilled file is kept; zero means 7 days, a
	// negative value never deletes.
	SpillRetention time.Duration
	// Sessions enables the session_id argument: a named shell held open between
	// calls, pooled per TOOL (spec §2.7k). Needs a terminal backend and RegisterCloser.
	Sessions bool
	// Background, when set, enables the background argument: a call asking for
	// it is handed here after decoding, the policy and the approval, and what it
	// returns is the call's result (spec §2.7w). Requires SpillDir.
	Background func(ctx context.Context, tc *agents.ToolContext, cmd *Command) (agents.ToolResult, error)
	// BackgroundMaxTimeout caps a background command; zero means 60 minutes. A
	// call whose timeout_seconds is 0 gets the cap.
	BackgroundMaxTimeout time.Duration
	// Policy filters commands before the approval gate (spec §2.7j).
	Policy Policy
	// NeedsApprovalFunc, when set, is the tool's per-call approval gate, given
	// the command's argsJSON and the model-assigned callID; nil never gates.
	NeedsApprovalFunc func(ctx context.Context, rc *agents.RunContext, argsJSON string, callID string) (bool, error)

	// RegisterCloser, when set, receives the closer that releases every shell the
	// Sessions pool holds open; wire it to whatever owns the sandbox's lifetime.
	RegisterCloser func(io.Closer)
}

const (
	defaultMaxTimeout           = 10 * time.Minute
	defaultBackgroundMaxTimeout = 60 * time.Minute
)

// DefaultDescription is what an empty Description falls back to.
func (CodeToolConfig) DefaultDescription() string {
	return "Execute a shell command in a sandboxed environment and return its stdout, stderr and exit code. " +
		"The command is run via bash -c. " +
		"Use timeout_seconds to override the default timeout for long-running commands. " +
		"Use workdir to run the command in a specific directory (relative paths are resolved from /workspace)."
}

func (c CodeToolConfig) withDefaults() CodeToolConfig {
	c.Name = cmp.Or(c.Name, "exec_command")
	if c.Description == "" {
		c.Description = c.DefaultDescription()
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8192
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = defaultMaxTimeout
	}
	if c.BackgroundMaxTimeout <= 0 {
		c.BackgroundMaxTimeout = defaultBackgroundMaxTimeout
	}
	return c
}

// lenientString decodes the JSON zero-value sentinels null, 0 and false to "";
// any other non-string scalar is rejected and fed back as text (spec §2.7l).
type lenientString string

func (s *lenientString) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err == nil {
		*s = lenientString(v)
		return nil
	}
	switch lit := string(bytes.TrimSpace(data)); lit {
	case "null", "0", "false":
		*s = ""
		return nil
	default:
		return fmt.Errorf("cannot decode %s as a string", lit)
	}
}

// lenientBool decodes null and 0 as false and 1 as true besides the booleans;
// anything else is rejected and fed back as text (spec §2.7l).
type lenientBool bool

func (b *lenientBool) UnmarshalJSON(data []byte) error {
	switch lit := string(bytes.TrimSpace(data)); lit {
	case "true", "1":
		*b = true
	case "false", "0", "null":
		*b = false
	default:
		return fmt.Errorf("cannot decode %s as a boolean", lit)
	}
	return nil
}

// codeToolArgs is the full argument set and always the decode target; the
// schema drops session_id and background when the tool does not offer them,
// and one sent anyway (non-strict backend) is gated in OnInvoke.
type codeToolArgs struct {
	Cmd            string        `json:"cmd"             jsonschema:"the shell command to execute (passed to bash -c)"`
	TimeoutSeconds int           `json:"timeout_seconds" jsonschema:"execution timeout in seconds; 0 uses the default"`
	Workdir        lenientString `json:"workdir"         jsonschema:"working directory for the command; empty uses the sandbox default"`
	SessionID      lenientString `json:"session_id"      jsonschema:"reuse a persistent shell by name, so cd, exported variables and an activated environment survive between calls; empty runs in a fresh shell"`
	Background     lenientBool   `json:"background"      jsonschema:"run in the background: returns at once with a handle while the command keeps running, its output saved to files, and the result reaches you in a later turn — so finish your reply and end your turn. For commands that take minutes (builds, test suites, downloads). Not combined with session_id; timeout_seconds 0 means the background cap"`
}

// dropArg removes one property an object schema offers, and its required entry.
func dropArg(schema map[string]any, name string) {
	if props, ok := schema["properties"].(map[string]any); ok {
		delete(props, name)
	}
	switch req := schema["required"].(type) {
	case []any:
		schema["required"] = slices.DeleteFunc(req, func(v any) bool { return v == name })
	case []string:
		schema["required"] = slices.DeleteFunc(req, func(v string) bool { return v == name })
	}
}

// CodeTool wraps a Sandbox as a function tool: the model's shell command runs
// via bash -c, and stdout, stderr and the exit code come back as text, a
// non-zero exit or a timeout included.
func CodeTool(sb Sandbox, cfg CodeToolConfig) *agents.Tool {
	cfg = cfg.withDefaults()
	// The pool exists only when named sessions do.
	var sessions *sessionPool
	if cfg.Sessions {
		sessions = newSessionPool()
		if cfg.RegisterCloser != nil {
			cfg.RegisterCloser(sessions)
		}
	}
	schema, err := agents.SchemaFor[codeToolArgs](true)
	if err != nil {
		// A deterministic programmer error (the argument type is fixed at
		// compile time), surfaced at construction like NewTool's.
		panic(fmt.Sprintf("sandbox: CodeTool(%q): schema generation failed: %v", cfg.Name, err))
	}
	// The schema offers only what the tool does — see spec §2.7k, §2.7w.
	if !cfg.Sessions {
		dropArg(schema, "session_id")
	}
	if cfg.Background == nil {
		dropArg(schema, "background")
	} else if cfg.SpillDir == "" {
		panic(fmt.Sprintf("sandbox: CodeTool(%q): Background requires SpillDir", cfg.Name))
	}
	if cfg.SpillDir != "" {
		// ExecRequest.Files lands under the working directory on every backend.
		clean := path.Clean(cfg.SpillDir)
		if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			panic(fmt.Sprintf("sandbox: CodeTool(%q): SpillDir %q is not a relative path under the working directory", cfg.Name, cfg.SpillDir))
		}
		cfg.SpillDir = clean
	}
	// Compiled once and shared read-only by both closures: the runner runs a
	// turn's tool calls in parallel. The zero Policy refuses nothing.
	compiled, policyErr := cfg.Policy.compile()
	checkPolicy := func(cmd string) error {
		if policyErr != nil {
			return policyErr // an uncompilable policy refuses everything (spec §2.7j)
		}
		return compiled.check(cmd)
	}
	// A call OnInvoke will refuse as text — a policy veto or malformed
	// arguments — reports "no approval needed" (spec §2.7j, §2.7l).
	needsApproval := cfg.NeedsApprovalFunc
	if needsApproval != nil {
		inner := needsApproval
		needsApproval = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
			var args codeToolArgs
			if json.Unmarshal([]byte(argsJSON), &args) != nil || checkPolicy(args.Cmd) != nil {
				return false, nil //nolint:nilerr // the veto is deliberate: OnInvoke refuses this call as text
			}
			return inner(ctx, rc, argsJSON, callID)
		}
	}
	return &agents.Tool{
		Name:              cfg.Name,
		Description:       cfg.Description,
		ParamsJSONSchema:  schema,
		Strict:            true,
		NeedsApprovalFunc: needsApproval,
		OnInvoke: func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (agents.ToolResult, error) {
			var args codeToolArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				// Refused as text, not an error (spec §2.7l).
				res := agents.TextResult(fmt.Sprintf("invalid arguments: %v", err))
				res.IsError = true
				return res, nil
			}

			timeout := cmp.Or(cfg.Timeout, DefaultTimeout)
			if args.TimeoutSeconds > 0 {
				timeout = min(time.Duration(args.TimeoutSeconds)*time.Second, cfg.MaxTimeout)
			}

			// Refused as TEXT naming the rule, not an error (spec §2.7j).
			if err := checkPolicy(args.Cmd); err != nil {
				return agents.TextResult(err.Error()).WithDisplay("terminal").
					WithDetails(map[string]any{"command": args.Cmd, "refused": true}), nil
			}

			cmd := args.Cmd
			if args.Workdir != "" {
				cmd = "cd " + ShellQuote(string(args.Workdir)) + " && " + cmd
			}
			var callID string
			if tc != nil {
				callID = tc.ToolCallID
			}
			if args.Background {
				return cfg.background(ctx, tc, args, cmd, callID)
			}

			// Instrumented here, the one place every backend is reached through.
			span, ctx := tracing.StartSpanFrom(ctx, "sandbox.exec", tracing.SpanTypeSandbox,
				map[string]any{"tool": cfg.Name, "timeout_ms": timeout.Milliseconds()})
			defer span.Finish()

			// A named session runs in a shell held open from the last call
			// (spec §2.7k).
			if session := string(args.SessionID); cfg.Sessions && session != "" {
				out, code, err := sessions.run(ctx, sb, session, cmd, timeout)
				out = out.trimRight("\n")
				cfg.spill(ctx, sb, span, callID, spillStream{".out", &out})
				if err != nil {
					// The partial output still reaches the model (spec §2.7k).
					span.SetError(err.Error(), nil)
					return agents.TextResult(fmt.Sprintf("session %q: %v\n%s",
						session, err, formatSessionResult(out, code, cfg.MaxOutputBytes))).
						WithDisplay("terminal").
						WithDetails(map[string]any{"command": cmd, "session_id": session}), nil
				}
				span.Set("exit_code", code)
				return agents.TextResult(formatSessionResult(out, code, cfg.MaxOutputBytes)).
					WithDisplay("terminal").
					WithDetails(map[string]any{
						"command":    cmd,
						"exit_code":  code,
						"session_id": session,
					}), nil
			}

			req := ExecRequest{Cmd: []string{"bash", "-c", cmd}, Timeout: timeout}
			var res *ExecResult
			var stdout, stderr captured
			var err error
			if streamer, ok := sb.(ExecStreamer); ok && tc != nil {
				// Streamed as progress events; the model still gets only the
				// final result, built from the head-and-tail capture (spec §2.7v).
				maxOut := int(req.EffectiveMaxOutputBytes())
				outBuf, errBuf := newHeadTail(maxOut), newHeadTail(maxOut)
				w := &progressWriter{tc: tc, cmd: cmd}
				res, err = streamer.ExecStream(ctx,
					req, io.MultiWriter(outBuf, w), io.MultiWriter(errBuf, w))
				w.flush()
				stdout, stderr = outBuf.captured(), errBuf.captured()
			} else {
				res, err = sb.Exec(ctx, req)
				if res != nil {
					stdout, stderr = whole(res.Stdout), whole(res.Stderr)
				}
			}
			if err != nil {
				span.SetError(err.Error(), nil)
				return agents.ToolResult{}, agents.Classify(agents.CodeSandboxExec, fmt.Errorf("code tool %q: %w", cfg.Name, err))
			}
			span.Set("exit_code", res.ExitCode)
			span.Set("timed_out", res.TimedOut)
			span.Set("stdout_bytes", stdout.Total)
			span.Set("stderr_bytes", stderr.Total)
			cfg.spill(ctx, sb, span, callID, spillStream{".stdout", &stdout}, spillStream{".stderr", &stderr})
			// Details carry the exit code too, so a UI need not parse the text.
			return agents.TextResult(formatResult(res, stdout, stderr, cfg.MaxOutputBytes)).
				WithDisplay("terminal").
				WithDetails(map[string]any{
					"command":   cmd,
					"exit_code": res.ExitCode,
					"timed_out": res.TimedOut,
				}), nil
		},
	}
}

// background hands a background call to the host, or refuses it as text; a
// refusal never runs the command in the foreground instead (spec §2.7w).
func (c CodeToolConfig) background(ctx context.Context, tc *agents.ToolContext, args codeToolArgs, line, callID string) (agents.ToolResult, error) {
	refuse := func(msg string) (agents.ToolResult, error) {
		res := agents.TextResult(msg).WithDisplay("terminal").
			WithDetails(map[string]any{"command": line, "refused": true})
		res.IsError = true
		return res, nil
	}
	switch {
	case c.Background == nil:
		return refuse("background is not available here; run the command in the foreground")
	case c.Sessions && args.SessionID != "":
		return refuse("background runs in a fresh shell; drop session_id")
	}
	timeout := c.BackgroundMaxTimeout
	if args.TimeoutSeconds > 0 {
		timeout = min(time.Duration(args.TimeoutSeconds)*time.Second, timeout)
	}
	return c.Background(ctx, tc, &Command{Line: line, Timeout: timeout, CallID: callID, cfg: c})
}

// formatResult renders an exec's outcome as the string sent back to the model.
func formatResult(res *ExecResult, stdout, stderr captured, limit int) string {
	var b strings.Builder
	if res.TimedOut {
		b.WriteString("[timed out]\n")
	}
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if stdout.Total > 0 {
		b.WriteString("stdout:\n")
		b.WriteString(stdout.render(limit))
		b.WriteString("\n")
	}
	if stderr.Total > 0 {
		b.WriteString("stderr:\n")
		b.WriteString(stderr.render(limit))
		b.WriteString("\n")
	}
	return b.String()
}

// truncateWithInfo cuts s to at most limit bytes, keeping the head (60%) and
// the tail (40%) — progress first, failure summary last — on rune boundaries.
func truncateWithInfo(s string, limit int) string { return whole(s).render(limit) }

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

// progressWriter turns sandbox output into tool-progress events per completed
// line. One writer serves stdout AND stderr (separate goroutines), hence mu.
type progressWriter struct {
	tc  *agents.ToolContext
	cmd string

	mu  sync.Mutex
	buf []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	w.mu.Lock()
	defer w.mu.Unlock()
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
