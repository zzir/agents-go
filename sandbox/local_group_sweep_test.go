//go:build unix

package sandbox

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A normal (non-timeout) exit must NOT sweep the process group: by then the
// group leader has been reaped and its PID may have been reused, so a sweep
// could SIGKILL an unrelated process group. Observable contract: a grandchild
// backgrounded by a successfully-exiting command survives Exec.
// (local_unix_test.go covers the inverse: on timeout the group IS killed.)
func TestLocalSandbox_NormalExitDoesNotKillProcessGroup(t *testing.T) {
	sb := NewLocal()
	// The shell prints the backgrounded grandchild's pid and exits 0
	// immediately; only the grandchild keeps the pipes open, which WaitDelay
	// unblocks after ~2s.
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd:     []string{"sh", "-c", "sleep 30 & echo $!"},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		t.Fatalf("TimedOut = %v, exit = %d; want false, 0", res.TimedOut, res.ExitCode)
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if perr != nil {
		t.Fatalf("could not parse grandchild pid from stdout %q: %v", res.Stdout, perr)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	// The grandchild must still be alive: signal 0 probes existence.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("grandchild %d is gone after a normal exit (err %v): the group sweep must only run on timeout", pid, err)
	}
}
