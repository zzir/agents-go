package sandbox

import (
	"context"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
)

// Command is one exec_command call decoded, policy-checked and approved, for
// the host to run in the background (spec §2.7w).
type Command struct {
	// Line is the shell line as exec_command would run it, workdir applied.
	Line string
	// Timeout bounds the run; the sandbox kills the command past it.
	Timeout time.Duration
	// CallID is the model's id for the call; the output files carry it.
	CallID string

	cfg CodeToolConfig // the tool's spill directory, output cap and name
}

// Run executes the command on sb with stdout and stderr redirected to files
// under SpillDir, blocks until it ends or ctx is done, and returns the
// model-facing result, a verdict line first. A nil error covers a non-zero
// exit and a timeout; an error is sandbox infrastructure failure.
func (c *Command) Run(ctx context.Context, sb Sandbox) (agents.ToolResult, error) {
	span, ctx := tracing.StartSpanFrom(ctx, "sandbox.exec", tracing.SpanTypeSandbox,
		map[string]any{"tool": c.cfg.Name, "timeout_ms": c.Timeout.Milliseconds(), "background": true})
	defer span.Finish()
	fail := func(err error) (agents.ToolResult, error) {
		span.SetError(err.Error(), nil)
		return agents.ToolResult{}, agents.Classify(agents.CodeSandboxExec, fmt.Errorf("code tool %q: %w", c.cfg.Name, err))
	}

	stamp := time.Now().UTC().Format("20060102T150405Z") + "-" + spillID(c.CallID)
	outPath := path.Join(c.cfg.SpillDir, stamp+".stdout")
	errPath := path.Join(c.cfg.SpillDir, stamp+".stderr")
	// The redirection precedes the line's own cd, so the files live under the
	// sandbox's default directory; one that fails exits 125 with the line unrun.
	script := spillScript(c.cfg.SpillDir, c.cfg.SpillRetention) +
		"; exec >" + ShellQuote(outPath) + " 2>" + ShellQuote(errPath) + " || exit 125; " + c.Line
	start := time.Now()
	res, err := sb.Exec(ctx, ExecRequest{Cmd: []string{"bash", "-c", script}, Timeout: c.Timeout})
	if err != nil {
		return fail(err)
	}
	elapsed := time.Since(start)
	stdout, stderr, err := c.readBack(ctx, sb, outPath, errPath)
	if err != nil {
		return fail(err)
	}
	span.Set("exit_code", res.ExitCode)
	span.Set("timed_out", res.TimedOut)
	span.Set("stdout_bytes", stdout.Total)
	span.Set("stderr_bytes", stderr.Total)
	span.Set("spill_path", []string{outPath, errPath})

	var hdr strings.Builder
	if res.TimedOut {
		fmt.Fprintf(&hdr, "timed out after %s", c.Timeout.Round(time.Second))
	} else {
		fmt.Fprintf(&hdr, "exit %d · %s", res.ExitCode, elapsed.Round(100*time.Millisecond))
	}
	fmt.Fprintf(&hdr, " · stdout %s · stderr %s", fmtBytes(stdout.Total), fmtBytes(stderr.Total))
	var files []string
	for _, s := range []captured{stdout, stderr} {
		if s.Total > 0 {
			files = append(files, s.Saved)
		}
	}
	if len(files) > 0 {
		hdr.WriteString(" · full output: " + strings.Join(files, ", "))
	}
	return agents.TextResult(hdr.String() + "\n" + formatResult(res, stdout, stderr, c.cfg.MaxOutputBytes)).
		WithDisplay("terminal").
		WithDetails(map[string]any{
			"command":      c.Line,
			"exit_code":    res.ExitCode,
			"timed_out":    res.TimedOut,
			"duration_ms":  elapsed.Milliseconds(),
			"stdout_path":  outPath,
			"stderr_path":  errPath,
			"stdout_bytes": stdout.Total,
			"stderr_bytes": stderr.Total,
		}), nil
}

// readBack sizes both files, then prints each whole under the cap, else its
// head and tail; the sizes come first, so the split is by count, binary-safe.
func (c *Command) readBack(ctx context.Context, sb Sandbox, outPath, errPath string) (stdout, stderr captured, err error) {
	limit := c.cfg.MaxOutputBytes
	head := limit * 3 / 5
	tail := limit - head
	var b strings.Builder
	for _, p := range []string{outPath, errPath} {
		q := ShellQuote(p)
		fmt.Fprintf(&b, "if [ -e %s ]; then echo $(($(wc -c < %s))); else echo -1; fi; ", q, q)
	}
	for _, p := range []string{outPath, errPath} {
		q := ShellQuote(p)
		fmt.Fprintf(&b, "if [ -e %s ]; then if [ $(($(wc -c < %s))) -le %d ]; then cat %s; else head -c %d %s; tail -c %d %s; fi; fi; ",
			q, q, limit, q, head, q, tail, q)
	}
	res, err := sb.Exec(ctx, ExecRequest{
		Cmd:            []string{"sh", "-c", b.String()},
		Timeout:        DefaultTimeout,
		MaxOutputBytes: int64(2*limit + 256),
	})
	if err != nil {
		return captured{}, captured{}, err
	}
	if res.ExitCode != 0 {
		return captured{}, captured{}, fmt.Errorf("reading the output back: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	rest := res.Stdout
	var sizes [2]int64
	for i := range sizes {
		line, after, ok := strings.Cut(rest, "\n")
		if !ok {
			return captured{}, captured{}, fmt.Errorf("reading the output back: malformed size line %q", rest)
		}
		if sizes[i], err = strconv.ParseInt(strings.TrimSpace(line), 10, 64); err != nil {
			return captured{}, captured{}, fmt.Errorf("reading the output back: %w", err)
		}
		rest = after
	}
	if sizes[0] < 0 || sizes[1] < 0 {
		return captured{}, captured{}, fmt.Errorf("the command's output was not redirected to %s", c.cfg.SpillDir)
	}
	take := func(n int64, p string) captured {
		if n <= int64(limit) {
			s := rest[:min(len(rest), int(n))]
			rest = rest[len(s):]
			return captured{Head: s, Total: n, Saved: p}
		}
		h := rest[:min(len(rest), head)]
		rest = rest[len(h):]
		t := rest[:min(len(rest), tail)]
		rest = rest[len(t):]
		return captured{Head: h, Tail: t, Total: n, Saved: p}
	}
	stdout = take(sizes[0], outPath)
	stderr = take(sizes[1], errPath)
	return stdout, stderr, nil
}

// fmtBytes renders a byte count for the verdict line: 512 B, 18 KB, 2.3 MB.
func fmtBytes(n int64) string {
	trim := func(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return trim(float64(n)/1024) + " KB"
	default:
		return trim(float64(n)/(1<<20)) + " MB"
	}
}
