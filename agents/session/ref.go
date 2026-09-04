package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Ref addresses one session: one generation of one id, so a handle to a
// deleted-then-recreated id cannot reach its replacement. A ref with no
// generation is the direct scope, where the id names the storage — spec §2.5e2.
type Ref struct {
	// ID is the session's name, as a caller knows it.
	ID string
	// Gen distinguishes this generation of that name from the ones before.
	// Empty is the direct scope; see above.
	Gen string
}

// Direct returns a ref for the scope where the id names the storage.
func Direct(id string) Ref { return Ref{ID: id} }

// String renders the ref for logs and errors.
func (r Ref) String() string {
	if r.Gen == "" {
		return r.ID
	}
	return r.ID + "@" + r.Gen
}

// NewGeneration mints a value no previous generation of any id has held.
func NewGeneration() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("agents: minting a session generation: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// NewSessionID mints an id for a Repo.Create call that did not supply one. The
// random suffix beside the timestamp keeps two id-less Creates in one clock
// tick from colliding.
func NewSessionID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("sess_%d_%s", time.Now().UnixNano(), hex.EncodeToString(buf[:]))
}
