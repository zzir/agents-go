package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Ref addresses one session: one generation of one id. The generation is what
// keeps a handle to a deleted-then-recreated id from reading or writing its
// replacement — two conversations sharing a history, silently. Making it part
// of the address means a function that takes a Ref cannot be handed a bare id.
//
// A ref with no generation is the direct scope, where the id names the storage
// (filesession.New, sessions.New). It is a scope, NOT a wildcard: a repo's
// delete does not reach it, and its writes do not reach a repo's sessions.
type Ref struct {
	// ID is the session's name, as a caller knows it.
	ID string
	// Gen distinguishes this generation of that name from the ones before.
	// Empty is the direct scope; see above.
	Gen string
}

// Direct returns a ref for the scope where the id names the storage.
func Direct(id string) Ref { return Ref{ID: id} }

// IsDirect reports whether r addresses the scope where the id names the
// storage, rather than one generation of a repo-managed session.
func (r Ref) IsDirect() bool { return r.Gen == "" }

// String renders the ref for logs and errors.
func (r Ref) String() string {
	if r.IsDirect() {
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
