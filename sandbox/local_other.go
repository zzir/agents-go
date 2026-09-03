//go:build !unix

package sandbox

import "os/exec"

// setProcessGroup is a no-op without Unix process groups; the default
// CommandContext cancellation applies, and Cmd.WaitDelay still bounds the pipes.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup is a no-op on platforms without Unix process groups.
func killProcessGroup(pid int) error { return nil }
