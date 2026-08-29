// Integration tests for the persistent-mode exec timeout. Unlike
// integration_test.go (which is opt-in via the docker_integration build tag),
// these probe the environment and skip when no Docker daemon is reachable or
// the test image is not available locally, so plain `go test ./...` exercises
// them on machines that have Docker without ever pulling images.
package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/zzir/agents-go/sandbox"
)

// testImage must provide a coreutils sleep (for the "sleep infinity"
// persistent entrypoint) and a POSIX sh. It matches integration_test.go.
const testImage = "python:3.12-slim"

// newPersistentSandbox returns a persistent-mode sandbox against the local
// daemon, skipping the test when the daemon or the image is unavailable.
func newPersistentSandbox(t *testing.T, opts Options) *Sandbox {
	t.Helper()
	probe, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	defer probe.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := probe.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if _, err := probe.ImageInspect(ctx, opts.Image); err != nil {
		t.Skipf("image %s not available locally: %v", opts.Image, err)
	}

	sb, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// Persistent-mode exec must honor the request timeout even when the command
// never exits and stops producing output ("sleep 20", "sleep infinity"): the
// hijacked attach connection has no deadline of its own, so the call must
// return a timeout result in roughly Timeout time and kill the exec process.
func TestPersistentExecTimeout(t *testing.T) {
	sb := newPersistentSandbox(t, Options{Image: testImage, Persistent: true})

	start := time.Now()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		// exec keeps it a single process so the post-timeout kill is
		// pid-exact; "started" proves pre-timeout output is preserved.
		Cmd:     []string{"sh", "-c", "echo started; exec sleep 20"},
		Timeout: 2 * time.Second,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1", res.TimedOut, res.ExitCode)
	}
	// Well under the 20s the command would need: proves the deadline
	// interrupted the blocked read (the container create adds a few seconds
	// of setup on top of the 2s timeout).
	if elapsed >= 10*time.Second {
		t.Errorf("Exec took %v; the timeout did not interrupt the attach read", elapsed)
	}
	if !strings.Contains(res.Stdout, "started") {
		t.Errorf("stdout = %q, want pre-timeout output preserved", res.Stdout)
	}

	// killExec must have terminated the timed-out process inside the
	// container (it must not linger for the full 20s).
	waitGone(t, sb, "sleep 20")

	// The persistent container stays usable after a timed-out exec.
	res2, err := sb.Exec(context.Background(), sandbox.ExecRequest{Cmd: []string{"echo", "still-alive"}})
	if err != nil {
		t.Fatal(err)
	}
	if res2.ExitCode != 0 || !strings.Contains(res2.Stdout, "still-alive") {
		t.Errorf("follow-up exec: exit = %d, stdout = %q", res2.ExitCode, res2.Stdout)
	}
}

// The streaming counterpart of TestPersistentExecTimeout: ExecStream shares
// the same hijacked-connection read and needs the same interruption.
func TestPersistentExecStreamTimeout(t *testing.T) {
	sb := newPersistentSandbox(t, Options{Image: testImage, Persistent: true})

	var stdout, stderr strings.Builder
	start := time.Now()
	res, err := sb.ExecStream(context.Background(), sandbox.ExecRequest{
		Cmd:     []string{"sh", "-c", "echo streamed; exec sleep 20"},
		Timeout: 2 * time.Second,
	}, &stdout, &stderr)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1", res.TimedOut, res.ExitCode)
	}
	if elapsed >= 10*time.Second {
		t.Errorf("ExecStream took %v; the timeout did not interrupt the attach read", elapsed)
	}
	if !strings.Contains(stdout.String(), "streamed") {
		t.Errorf("streamed stdout = %q, want pre-timeout output", stdout.String())
	}
	waitGone(t, sb, "sleep 20")
}

// A persistent exec that floods BOTH output streams past the cap and then
// keeps running must not be mistaken for a finished one. Exec and ExecStream
// share one core that reads the attach stream to its end; the exec-only path
// used to stop as soon as both capped sinks were full and then read the exit
// code of a still-running exec — reporting "exit 0" while the command kept
// running in the container, unkilled.
func TestPersistentExecFloodedStreamsKeepRunning(t *testing.T) {
	sb := newPersistentSandbox(t, Options{Image: testImage, Persistent: true})

	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		// Both streams overshoot MaxOutputBytes, then the process goes quiet
		// without exiting.
		Cmd:            []string{"sh", "-c", "yes o | head -c 20000; yes e | head -c 20000 >&2; exec sleep 20"},
		Timeout:        3 * time.Second,
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1 (a still-running exec must never report an exit code)",
			res.TimedOut, res.ExitCode)
	}
	if len(res.Stdout) != 1024 || len(res.Stderr) != 1024 {
		t.Errorf("stdout = %d bytes, stderr = %d bytes; want both capped at 1024", len(res.Stdout), len(res.Stderr))
	}
	// The timeout path must also have killed it, rather than leaving it behind.
	waitGone(t, sb, "sleep 20")
}

// ReadFile through the persistent (CopyFromContainer) path must enforce
// MaxReadFileBytes.
func TestPersistentReadFileLimit(t *testing.T) {
	sb := newPersistentSandbox(t, Options{Image: testImage, Persistent: true, MaxReadFileBytes: 1024})

	ctx := context.Background()
	if err := sb.WriteFile(ctx, "big.bin", make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.ReadFile(ctx, "big.bin"); !errors.Is(err, sandbox.ErrReadLimitExceeded) {
		t.Errorf("ReadFile(big.bin) err = %v, want sandbox.ErrReadLimitExceeded", err)
	}
	if err := sb.WriteFile(ctx, "small.txt", []byte("fits")); err != nil {
		t.Fatal(err)
	}
	data, err := sb.ReadFile(ctx, "small.txt")
	if err != nil {
		t.Fatalf("ReadFile(small.txt): %v", err)
	}
	if string(data) != "fits" {
		t.Errorf("data = %q", data)
	}
}

// WriteFile must leave existing parent directories alone: the seeded tar used
// to carry dir headers (mode 0777, uid/gid 0, epoch mtime) that the daemon's
// untar re-applied to /workspace and every parent on the path.
func TestPersistentWriteFileKeepsParentDirMetadata(t *testing.T) {
	sb := newPersistentSandbox(t, Options{Image: testImage, Persistent: true})
	ctx := t.Context()

	mustExec := func(cmd string) string {
		t.Helper()
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", cmd}})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("%s: exit %d, stderr %q", cmd, res.ExitCode, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// chown is unavailable (CapDrop ALL), so the mode — 750, which the old dir
	// headers reset to 777 — and the epoch mtime carry the assertion.
	mustExec("mkdir -p /workspace/keep && chmod 750 /workspace/keep")
	workspaceBefore := mustExec("stat -c '%a %u %g' /workspace")
	rootBefore := mustExec("stat -c '%a %u %g' /root")

	if err := sb.WriteFile(ctx, "keep/sub/new.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got, err := sb.ReadFile(ctx, "keep/sub/new.txt"); err != nil || string(got) != "hi" {
		t.Fatalf("read back = %q, %v; want hi", got, err)
	}
	if got := mustExec("stat -c %a /workspace/keep"); got != "750" {
		t.Errorf("keep mode = %q, want 750 (WriteFile reset the existing dir)", got)
	}
	if got := mustExec("stat -c %Y /workspace/keep"); got == "0" {
		t.Error("keep mtime reset to the tar's epoch")
	}
	if got := mustExec("stat -c '%a %u %g' /workspace"); got != workspaceBefore {
		t.Errorf("/workspace = %q, want %q", got, workspaceBefore)
	}

	// An absolute path must not clobber ITS parents either (e.g. /root).
	if err := sb.WriteFile(ctx, "/root/probe/x.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := mustExec("stat -c '%a %u %g' /root"); got != rootBefore {
		t.Errorf("/root = %q, want %q", got, rootBefore)
	}
}

// waitGone polls the container's /proc until no process command line contains
// needle (the entrypoint "sleep infinity" never matches "sleep 20").
func waitGone(t *testing.T, sb *Sandbox, needle string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Cmd: []string{"sh", "-c", `for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' <"$f" 2>/dev/null; echo; done`},
		})
		if err != nil {
			t.Fatalf("proc scan: %v", err)
		}
		if !strings.Contains(res.Stdout, needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %q still running in the container:\n%s", needle, res.Stdout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
