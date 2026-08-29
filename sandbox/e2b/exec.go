package e2b

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zzir/agents-go/sandbox"
)

// processEvent is one frame of a process stream, in the camelCase protojson
// emits. Exactly one of the three is set per frame.
type processEvent struct {
	Event struct {
		Start *struct {
			PID uint32 `json:"pid"`
		} `json:"start"`
		Data *struct {
			// bytes fields arrive base64-encoded.
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
			PTY    string `json:"pty"`
		} `json:"data"`
		End *struct {
			// sint32 renders as a number; a service that renders it as a
			// string is tolerated by json.Number.
			ExitCode json.Number `json:"exitCode"`
			Exited   bool        `json:"exited"`
			Status   string      `json:"status"`
			Error    string      `json:"error"`
		} `json:"end"`
	} `json:"event"`
}

func (e processEvent) exitCode() int {
	if e.Event.End == nil {
		return 0
	}
	n, err := e.Event.End.ExitCode.Int64()
	if err != nil {
		return 0
	}
	return int(n)
}

// Exec runs a command and returns its captured output.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	stdout := &sandbox.CappedBuffer{Max: req.EffectiveMaxOutputBytes()}
	stderr := &sandbox.CappedBuffer{Max: req.EffectiveMaxOutputBytes()}
	res, err := s.ExecStream(ctx, req, stdout, stderr)
	if err != nil {
		return nil, err
	}
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	return res, nil
}

// ExecStream runs a command, writing its output to the caller's writers as it
// arrives.
func (s *Sandbox) ExecStream(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("e2b: ExecRequest.Cmd is empty")
	}
	if req.Stdin != "" {
		return nil, errors.New("e2b: ExecRequest.Stdin is not supported")
	}
	// A command bounded longer than the lease's refresh margin needs the lease
	// extended first, or the service kills the sandbox mid-command.
	if _, err := s.ensureFor(ctx, req.EffectiveTimeout()); err != nil {
		return nil, err
	}
	if err := s.writeRequestFiles(ctx, req.Files); err != nil {
		return nil, err
	}

	start := map[string]any{
		"process": map[string]any{
			"cmd":  req.Cmd[0],
			"args": req.Cmd[1:],
			"envs": sandbox.MergeEnv(s.opts.Env, req.Env),
			"cwd":  s.workDir(),
		},
	}

	// The timeout is the CALLER's deadline, enforced here: envd's Start
	// carries none. A deadline that fires cancels the stream, and the process
	// is signalled so it does not outlive the request inside the sandbox.
	runCtx, cancel := withTimeout(ctx, req.EffectiveTimeout())
	defer cancel()

	var pid uint32
	var sawEnd bool
	result := &sandbox.ExecResult{}
	err := s.stream(runCtx, procProcessStart, start, func(raw json.RawMessage) error {
		var ev processEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("e2b: decoding a process event: %w", err)
		}
		switch {
		case ev.Event.Start != nil:
			pid = ev.Event.Start.PID
		case ev.Event.Data != nil:
			if err := writeChunk(stdout, ev.Event.Data.Stdout); err != nil {
				return err
			}
			if err := writeChunk(stderr, ev.Event.Data.Stderr); err != nil {
				return err
			}
		case ev.Event.End != nil:
			result.ExitCode = ev.exitCode()
			sawEnd = true
		}
		return nil
	})
	if err != nil {
		// A deadline is not a failure: it is the sandbox's answer, reported
		// the way every backend reports one (spec §2.7m).
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			s.signal(context.WithoutCancel(ctx), pid, "SIGNAL_SIGKILL")
			return &sandbox.ExecResult{ExitCode: -1, TimedOut: true}, nil
		}
		// Any other failure — a caller cancel, a dropped stream, a decode error
		// — leaves a process that started still running in the sandbox; kill it
		// so it does not outlive the request (best-effort: the transport may be
		// the thing that failed).
		if pid != 0 {
			s.signal(context.WithoutCancel(ctx), pid, "SIGNAL_SIGKILL")
		}
		return nil, err
	}
	if !sawEnd {
		// The stream closed cleanly but the process never reported an exit
		// (reaped without an end frame, a paused or killed sandbox). Returning
		// ExitCode 0 here would tell the caller a command that never finished
		// succeeded.
		if pid != 0 {
			s.signal(context.WithoutCancel(ctx), pid, "SIGNAL_SIGKILL")
		}
		return nil, fmt.Errorf("e2b: the process stream ended without an exit status")
	}
	return result, nil
}

// writeChunk decodes one base64 output chunk into w. A chunk that will not
// decode is dropped rather than failing the command: partial output is worth
// more to the model than none. A WRITE failure is returned — the caller
// stopped taking output (a closed export pipe), so streaming on is waste.
func writeChunk(w io.Writer, encoded string) error {
	if encoded == "" || w == nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil //nolint:nilerr // see above
	}
	_, werr := w.Write(raw)
	return werr
}

// writeRequestFiles materializes ExecRequest.Files before the command runs.
func (s *Sandbox) writeRequestFiles(ctx context.Context, files map[string]string) error {
	for name, content := range files {
		if err := s.WriteFile(ctx, name, []byte(content)); err != nil {
			return err
		}
	}
	return nil
}

// signal best-effort kills a process the caller stopped waiting for. It bounds
// its own call: a hung envd must not hang the signal that is meant to clean up
// after one.
func (s *Sandbox) signal(ctx context.Context, pid uint32, sig string) {
	if pid == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, controlCallTimeout)
	defer cancel()
	_ = s.unary(ctx, procSendSignal, map[string]any{
		"process": map[string]any{"pid": pid},
		"signal":  sig,
	}, nil)
}

// ExportTar streams the working tree as a tar archive, produced by the
// sandbox itself: there is no host-side filesystem to read, and the tool is
// already in every image these run.
func (s *Sandbox) ExportTar(ctx context.Context, p string) (io.ReadCloser, error) {
	dir := s.workDir()
	if p != "" {
		dir = s.resolvePath(p)
	}
	pr, pw := io.Pipe()
	go func() {
		// -C the parent and name the leaf, so the archive carries one top
		// directory rather than absolute paths.
		script := "tar -cf - -C " + sandbox.ShellQuote(parentOf(dir)) + " " + sandbox.ShellQuote(leafOf(dir))
		res, err := s.ExecStream(ctx, sandbox.ExecRequest{
			Cmd: []string{"sh", "-c", script},
			// An export is not a command with a 30-second answer.
			Timeout: exportTimeout,
		}, pw, io.Discard)
		switch {
		case err != nil:
			_ = pw.CloseWithError(err)
		case res.ExitCode != 0:
			_ = pw.CloseWithError(fmt.Errorf("e2b: export %q: tar exited %d", dir, res.ExitCode))
		default:
			_ = pw.Close()
		}
	}()
	return pr, nil
}

func parentOf(p string) string {
	if i := strings.LastIndex(strings.TrimSuffix(p, "/"), "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

func leafOf(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
