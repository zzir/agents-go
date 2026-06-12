//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup makes the command lead its own process group and replaces
// the context cancellation with a kill of the whole group, so backgrounded
// grandchildren are killed together with the command on timeout instead of
// surviving and holding the stdout/stderr pipes open.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := killProcessGroup(cmd.Process.Pid)
		if errors.Is(err, syscall.ESRCH) {
			// The group is already gone: not an error worth surfacing.
			return os.ErrProcessDone
		}
		return err
	}
}

// killProcessGroup forcefully kills the process group led by pid.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
