package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Ref addresses one session: one generation of one id.
//
// # Why an id is not enough
//
// A session id names a session, not a place. Deleting an id and creating it
// again has to yield a session with storage of its own, or a handle the caller
// still holds for the deleted one reads what its replacement writes and writes
// into what it reads — two conversations sharing a history, silently.
//
// A backend that derives storage from the id alone (a filename, a
// `WHERE session_id = ?`) therefore needs something beside it. The mistake to
// avoid is making that a field each code path remembers to carry: every place
// that builds a handle by hand, resolves by id, or deletes by name is then a
// chance to forget, and forgetting is silent — the write lands somewhere, the
// delete removes something.
//
// So it is the address. A function that takes a Ref cannot be handed a
// bare id, and a delete written against one cannot remove a generation the
// caller did not mean. What used to be a rule enforced by review is a parameter.
//
// # The empty generation
//
// A ref with no generation is the scope of the constructors where the id names
// the STORAGE — memory.NewFileSession, sessions.New. Opening one twice is the
// same conversation, by design; it cannot tell "reopened" from "recreated", and
// that is what it is for.
//
// It is a scope like any other, NOT a wildcard. A repo's delete does not reach
// it, and its writes do not reach a repo's sessions. Reading it as "any
// generation" is how a repo delete came to empty a session it had never
// created.
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
//
// It is one function rather than one per backend because "what makes this
// generation different from the last" is the same question everywhere, and
// three backends inventing three answers is three chances to pick one that can
// repeat.
func NewGeneration() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("agents: minting a session generation: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// NewSessionID mints an id for a Repo.Create call that did not supply one.
//
// It is one function for the same reason NewGeneration is: every backend needs
// an id no concurrent Create can also pick, and each one answering that
// separately is another chance to pick an answer that can repeat. The random
// suffix beside the timestamp is what makes two id-less Creates in one clock
// tick differ — without it the second fails as "already exists", for no reason
// its caller can see.
func NewSessionID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("sess_%d_%s", time.Now().UnixNano(), hex.EncodeToString(buf[:]))
}
