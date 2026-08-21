//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// stopSignals are the signals that end the whole run. A child in its own
// process group no longer receives the terminal's Ctrl-C, nor a SIGTERM aimed
// at our group, so both have to be caught and turned into a cancellation.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// setProcessGroup makes the command lead its own process group and replaces the
// context cancellation with a kill of the whole group, so the example `go run`
// exec'd dies with it instead of surviving as an orphan.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group is already gone: not an error worth surfacing.
			return os.ErrProcessDone
		}
		return err
	}
}
