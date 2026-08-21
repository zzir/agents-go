//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// stopSignals are the signals that end the whole run. Only interrupt is
// portable; SIGTERM is not delivered everywhere off Unix.
var stopSignals = []os.Signal{os.Interrupt}

// setProcessGroup is a no-op on platforms without Unix process groups; the
// default exec.CommandContext cancellation (kill the direct child) applies,
// and Cmd.WaitDelay still bounds waiting on inherited pipes.
func setProcessGroup(cmd *exec.Cmd) {}
