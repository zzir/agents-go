package sandboxes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"sync"
)

// CommandTrust records a session's exec_command approval grants: an "approve
// all" flag, plus the set of exact commands (hash of cmd+workdir) the user
// approved for the rest of the session. The zero value is ready to use.
type CommandTrust struct {
	mu         sync.RWMutex
	approveAll bool
	approved   map[string]bool
}

func (t *CommandTrust) trusted(hash string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.approveAll || t.approved[hash]
}

// AllowCommand trusts one exact command (a CommandHash) for the session.
func (t *CommandTrust) AllowCommand(hash string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.approved == nil {
		t.approved = make(map[string]bool)
	}
	t.approved[hash] = true
}

// AllowAll trusts every command for the session.
func (t *CommandTrust) AllowAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.approveAll = true
}

// TrustStore maps a session id to its CommandTrust: in-memory on purpose, so
// trust survives interrupt/resume within a process and resets on restart.
type TrustStore struct {
	mu        sync.Mutex
	bySession map[string]*CommandTrust
	// withheld is the runs started without standing trust, each under the
	// session whose trust it was denied.
	withheld map[string]string
}

// NewTrustStore returns an empty store.
func NewTrustStore() *TrustStore {
	return &TrustStore{bySession: make(map[string]*CommandTrust), withheld: make(map[string]string)}
}

type trustWithheldKey struct{}

// WithoutStandingTrust marks a run's context so exec_command asks for every
// command, whatever its session granted — invariant 84.
func WithoutStandingTrust(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustWithheldKey{}, true)
}

func standingTrustWithheld(ctx context.Context) bool {
	withheld, _ := ctx.Value(trustWithheldKey{}).(bool)
	return withheld
}

// WithholdRun records that runID runs without sessionID's standing trust, so
// the work it starts can be told from a person's.
func (s *TrustStore) WithholdRun(sessionID, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.withheld[runID] = sessionID
}

// RunWithheld reports whether WithholdRun recorded runID.
func (s *TrustStore) RunWithheld(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.withheld[runID]
	return ok
}

// ForSession returns the session's trust, created empty on first use.
func (s *TrustStore) ForSession(id string) *CommandTrust {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.bySession[id]
	if t == nil {
		t = &CommandTrust{}
		s.bySession[id] = t
	}
	return t
}

// Forget drops a session's trust and its withheld runs — the session-delete
// path calls it, since the maps otherwise grow for the process lifetime.
func (s *TrustStore) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bySession, id)
	maps.DeleteFunc(s.withheld, func(_, sessionID string) bool { return sessionID == id })
}

// CommandHash canonicalizes an exec_command argsJSON to a stable key, so
// "approve this exact command" matches only a byte-identical (cmd, workdir)
// pair. It is exact, not prefix/substring: approving `go test` never green-lights
// `go test && rm -rf` — any change re-triggers approval.
func CommandHash(argsJSON string) string {
	var a struct {
		Cmd     string `json:"cmd"`
		Workdir string `json:"workdir"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	sum := sha256.Sum256([]byte(a.Cmd + "\x00" + a.Workdir))
	return hex.EncodeToString(sum[:])
}
