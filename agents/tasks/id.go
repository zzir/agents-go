package tasks

import (
	"crypto/rand"
	"encoding/hex"
)

// newID mints an identifier for a task, run or session.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tasks: reading randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
