//go:build linux

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processDead reports whether pid is gone or a zombie: a killed grandchild
// reparented to a PID 1 that never reaps it stays in /proc in state Z.
func processDead(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
	}
	// The state follows the parenthesised comm, which may itself hold spaces
	// and parentheses: parse from the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(stat[i+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}
