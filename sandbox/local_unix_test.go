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

func TestLocalSandbox_TimeoutKillsProcessGroup(t *testing.T) {
	sb := NewLocal()
	start := time.Now()
	// The shell prints the backgrounded grandchild's pid, then keeps running in
	// the foreground past the deadline. The timeout must kill the whole process
	// group: both the direct child and the backgrounded sleep.
	res, err := sb.Exec(context.Background(), ExecRequest{
		Cmd:     []string{"sh", "-c", "sleep 30 & echo $!; sleep 30"},
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Exec took %v; the grandchild held the pipes open", elapsed)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut = true")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1", res.ExitCode)
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if perr != nil {
		t.Fatalf("could not parse grandchild pid from stdout %q: %v", res.Stdout, perr)
	}
	// The grandchild must be gone (allow a moment for init to reap it).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return // killed and reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after timeout: it escaped the process-group kill", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
