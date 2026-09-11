//go:build docker_integration

// Run with: go test -tags docker_integration ./sandbox/docker
// Requires a reachable Docker daemon and the python:3.12-slim image
// (docker pull python:3.12-slim).
package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

func TestDockerSandbox_RunsPython(t *testing.T) {
	sb, err := New(Options{Image: "python:3.12-slim", Limits: sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Files: map[string]string{"main.py": "print('hello from docker')"},
		Cmd:   []string{"python", "main.py"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello from docker") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestDockerSandbox_WritableWorkdirAndTmp(t *testing.T) {
	sb, err := New(Options{Image: "python:3.12-slim"})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	// The process runs as the image's own user (spec §2.7o) with a read-only
	// root fs: the working directory and /tmp must still be writable,
	// including nested dirs.
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Files: map[string]string{
			"main.py":         "import pkg.helper\nopen('out.txt','w').write('cwd')\nopen('/tmp/t','w').write('tmp')\nprint('writable ok')",
			"pkg/helper.py":   "open('pkg/nested.txt','w').write('n')",
			"pkg/__init__.py": "",
		},
		Cmd: []string{"python", "main.py"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "writable ok") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestDockerSandbox_Timeout(t *testing.T) {
	sb, err := New(Options{Image: "python:3.12-slim"})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Files:   map[string]string{"main.py": "import time; print('started', flush=True); time.sleep(30)"},
		Cmd:     []string{"python", "main.py"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1", res.TimedOut, res.ExitCode)
	}
	// Logs must still be collected after the deadline.
	if !strings.Contains(res.Stdout, "started") {
		t.Errorf("stdout after timeout = %q, want pre-timeout output", res.Stdout)
	}
}

func TestDockerSandbox_NetworkIsolated(t *testing.T) {
	sb, _ := New(Options{Image: "python:3.12-slim"})
	defer sb.Close()
	// With networking disabled, a socket connection should fail.
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Files: map[string]string{"main.py": "import socket; socket.create_connection(('1.1.1.1',53),timeout=3)"},
		Cmd:   []string{"python", "main.py"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Error("expected network access to be blocked")
	}
}

// Two Sandboxes on one named container: the second adopts the first's; the
// first's Detach leaves it running for the second, where Close would stop it.
func TestDockerSandbox_DetachHandsTheContainerOver(t *testing.T) {
	ctx := context.Background()
	opts := Options{Image: "python:3.12-slim", Persistent: true, KeepOnClose: true, ContainerName: "agents-detach-" + newExecMarker()[:8]}
	first, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveManaged(ctx, opts, opts.ContainerName) })
	if _, err := first.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	second, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Detach(); err != nil {
		t.Fatal(err)
	}
	if st, err := second.Status(ctx); err != nil || st != sandbox.StateRunning {
		t.Fatalf("status after the first's Detach = %v, %v; want running", st, err)
	}
	if res, err := second.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"echo", "still here"}}); err != nil || !strings.Contains(res.Stdout, "still here") {
		t.Fatalf("exec after the hand-over = %+v, %v", res, err)
	}
}
