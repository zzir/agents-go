package tasks

import (
	"crypto/rand"
	"encoding/hex"
)

// newID mints an identifier for a task, run or session. crypto/rand.Read never
// fails (Go 1.24+ aborts if the OS source is unavailable).
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
