package tasks

import (
	"crypto/rand"
	"encoding/hex"
)

// newID mints an identifier for a task, run or session. As of Go 1.24
// crypto/rand.Read never fails (it aborts the program if the OS source is
// unavailable), so there is no error to handle.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
