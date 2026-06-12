//go:build !unix

package sandbox

import "os/exec"

// setProcessGroup is a no-op on platforms without Unix process groups; the
// default exec.CommandContext cancellation (kill the direct child) applies,
// and Cmd.WaitDelay still bounds waiting on inherited pipes.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup is a no-op on platforms without Unix process groups.
func killProcessGroup(pid int) error { return nil }
