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
	// withheld is the runs started without their session's standing trust.
	withheld map[string]withheldRun
}

// withheldRun is a run's own grants, under the session whose trust it was denied.
type withheldRun struct {
	sessionID string
	trust     *CommandTrust
}

// NewTrustStore returns an empty store.
func NewTrustStore() *TrustStore {
	return &TrustStore{bySession: make(map[string]*CommandTrust), withheld: make(map[string]withheldRun)}
}

type runTrustKey struct{}

// WithRunTrust marks a run's context so exec_command reads own instead of the
// session's trust — invariant 84.
func WithRunTrust(ctx context.Context, own *CommandTrust) context.Context {
	return context.WithValue(ctx, runTrustKey{}, own)
}

func runTrust(ctx context.Context) *CommandTrust {
	own, _ := ctx.Value(runTrustKey{}).(*CommandTrust)
	return own
}

// WithholdRun returns runID's own trust, empty the first time: the run reads it
// instead of sessionID's, and the work it starts can be told from a person's.
func (s *TrustStore) WithholdRun(sessionID, runID string) *CommandTrust {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.withheld[runID]
	if !ok {
		w = withheldRun{sessionID: sessionID, trust: &CommandTrust{}}
		s.withheld[runID] = w
	}
	return w.trust
}

// RunTrust returns the trust WithholdRun made for runID, or nil.
func (s *TrustStore) RunTrust(runID string) *CommandTrust {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withheld[runID].trust
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
	maps.DeleteFunc(s.withheld, func(_ string, w withheldRun) bool { return w.sessionID == id })
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
