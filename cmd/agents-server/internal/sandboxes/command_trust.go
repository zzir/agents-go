package sandboxes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func (t *CommandTrust) AllowCommand(hash string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.approved == nil {
		t.approved = make(map[string]bool)
	}
	t.approved[hash] = true
}

func (t *CommandTrust) AllowAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.approveAll = true
}

// TrustStore maps a session id to its CommandTrust. It is session-scoped and
// in-memory on purpose: trust survives interrupt/resume within a process and
// resets on restart (the command is simply re-approved, which is safe). It is
// never persisted — it's a per-session convenience, not a durable policy.
type TrustStore struct {
	mu        sync.Mutex
	bySession map[string]*CommandTrust
}

// NewTrustStore returns an empty store.
func NewTrustStore() *TrustStore {
	return &TrustStore{bySession: make(map[string]*CommandTrust)}
}

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
