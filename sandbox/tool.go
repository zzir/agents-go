package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// Sessions enables the session_id argument: a named shell held open between
	// calls, so `cd`, exported variables and an activated virtualenv survive.
	// Off by default (a held-open shell must be closed — see RegisterCloser)
	// and needs a backend with interactive terminals. The pool is the TOOL's,
	// shared by every run using it; build the tool per run to isolate them
	// (spec §2.7k).
	Sessions bool
	// Policy filters commands before the approval gate (spec §2.7j).
	Policy Policy
	// NeedsApprovalFunc, when set, is forwarded to the tool as its per-call
	// approval gate: given the command in argsJSON and the model-assigned callID
	// it decides whether this execution must be approved first. nil = never gate.
	NeedsApprovalFunc func(ctx context.Context, rc *agents.RunContext, argsJSON string, callID string) (bool, error)

	// RegisterCloser, when set, receives the closer that releases every named
	// shell the Sessions pool holds open; *agents.Tool has no close of its own.
	// Wire it to whatever owns the sandbox's lifetime and call Close there.
	RegisterCloser func(io.Closer)
}

const defaultMaxTimeout = 10 * time.Minute

// DefaultDescription is what an empty Description falls back to, exported so
// a caller can extend it rather than restate it.
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
	return c
}

// lenientString is a string that also accepts the JSON zero-value sentinels
// null, 0 and false, decoding each to ""; any other non-string scalar is
// rejected, which OnInvoke feeds back as correctable text (spec §2.7l).
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

// codeToolArgsNoSession is the schema advertised when Sessions is off: the
// model is never offered a session_id the tool would ignore.
type codeToolArgsNoSession struct {
	Cmd            string        `json:"cmd"             jsonschema:"the shell command to execute (passed to bash -c)"`
	TimeoutSeconds int           `json:"timeout_seconds" jsonschema:"execution timeout in seconds; 0 uses the default"`
	Workdir        lenientString `json:"workdir"         jsonschema:"working directory for the command; empty uses the sandbox default"`
}

// codeToolArgs is the full argument set and always the decode target: a
// session_id sent anyway (non-strict backend) decodes and is gated on
// cfg.Sessions in OnInvoke.
type codeToolArgs struct {
	codeToolArgsNoSession
	SessionID lenientString `json:"session_id" jsonschema:"reuse a persistent shell by name, so cd, exported variables and an activated environment survive between calls; empty runs in a fresh shell"`
}

// CodeTool wraps a Sandbox as a function tool. The model supplies a shell
// command which is executed via bash -c; stdout, stderr and exit code are
// returned as text. Execution errors (non-zero exit, timeout) are returned
// to the model as output so it can correct itself.
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
	// The schema advertises session_id only when Sessions is on — see spec §2.7k.
	var schema map[string]any
	var err error
	if cfg.Sessions {
		schema, err = agents.SchemaFor[codeToolArgs](true)
	} else {
		schema, err = agents.SchemaFor[codeToolArgsNoSession](true)
	}
	if err != nil {
		// A deterministic programmer error (both argument types are fixed at
		// compile time), surfaced at construction like NewTool's.
		panic(fmt.Sprintf("sandbox: CodeTool(%q): schema generation failed: %v", cfg.Name, err))
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
	// arguments — reports "no approval needed", so it never reaches a human
	// (spec §2.7j, §2.7l).
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
				// Refused as TEXT, not an error: an error return would abort
				// the run over a spelling slip (spec §2.7l).
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

			// Instrumented here, the one place every backend is reached through.
			span, ctx := tracing.StartSpanFrom(ctx, "sandbox.exec", tracing.SpanTypeSandbox,
				map[string]any{"tool": cfg.Name, "timeout_ms": timeout.Milliseconds()})
			defer span.Finish()

			// A named session runs in a shell held open from the last call
			// (spec §2.7k).
			if session := string(args.SessionID); cfg.Sessions && session != "" {
				out, code, err := sessions.run(ctx, sb, session, cmd, timeout)
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
			var err error
			if streamer, ok := sb.(ExecStreamer); ok && tc != nil {
				// Streamed as progress events; the model still gets only the
				// final result, built from the capped buffers below.
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
			// Details carry the exit code too, so a UI need not parse the text.
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

// truncateWithInfo cuts s to at most limit bytes, keeping the head (60%) and
// the tail (40%) and eliding the middle — a build prints its progress first
// and its failure summary last. Rune boundaries are respected on both sides.
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

// progressWriter turns a sandbox's output stream into tool-progress events,
// coalesced per completed line. One writer serves BOTH stdout and stderr,
// which most backends pump from separate goroutines, so the buffer is guarded.
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
