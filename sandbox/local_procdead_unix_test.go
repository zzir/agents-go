//go:build unix && !linux

package sandbox

import (
	"errors"
	"syscall"
)

// processDead reports whether pid is gone. Without /proc a zombie cannot be
// told from a live process; init reaps orphans promptly on these platforms.
func processDead(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
