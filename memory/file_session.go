// Package memory provides persistent Session implementations for the agents SDK.
//
// FileSession stores conversation history as a JSONL file (one entry per line),
// with zero external dependencies. It suits single-machine, moderate-volume use;
// for high concurrency or large histories, implement agents.Session against a
// database of your choice.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzir/agents-go/agents"
)

// pathLocks shares one mutex per session file path, so independently opened
// FileSession instances on the same file serialize their access. Keys are
// absolute cleaned paths.
//
// Entries are reference-counted: acquire registers interest before locking and
// release drops it after unlocking, deleting the entry once no holder or
// waiter remains. Long-lived processes that churn through many one-off
// sessions therefore do not accumulate stale mutexes. A plain map plus mutex
// suffices — lock traffic is one acquire per session operation, far below the
// contention sync.Map is designed for.
var (
	pathLocksMu sync.Mutex
	pathLocks   = make(map[string]*pathLock)
)

// pathLock is one entry in pathLocks: the per-path mutex plus the number of
// current holders and waiters, managed under pathLocksMu.
type pathLock struct {
	mu   sync.Mutex
	refs int
}

// lockKeyFor normalizes a session file path to its pathLocks key.
func lockKeyFor(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// acquire blocks until the per-path mutex for key is held and returns the
// release func that must be called exactly once to unlock it. Different keys
// proceed in parallel; the same key is mutually exclusive.
func acquire(key string) (release func()) {
	pathLocksMu.Lock()
	l := pathLocks[key]
	if l == nil {
		l = &pathLock{}
		pathLocks[key] = l
	}
	l.refs++
	pathLocksMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		pathLocksMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(pathLocks, key)
		}
		pathLocksMu.Unlock()
	}
}

// lockTableSize reports the number of live pathLocks entries. Test-only.
func lockTableSize() int {
	pathLocksMu.Lock()
	defer pathLocksMu.Unlock()
	return len(pathLocks)
}

// FileSession is a Session backed by a JSONL file: each conversation item is one
// line of JSON. One file holds one session; pass a directory plus a session ID
// to keep multiple conversations side by side. It is goroutine-safe within a
// process — including across multiple FileSession instances opened on the same
// path, which share a per-path lock. Cross-process access is not locked.
type FileSession struct {
	path    string
	lockKey string
}

// NewFileSession returns a session stored at dir/<sessionID>.jsonl, creating dir
// if needed. The session ID is sanitized for use as a filename; an id the
// sanitizer had to alter also gets a fingerprint of the ORIGINAL id in the
// name, so two different ids can never share a file. ("team a" and "team+a"
// both sanitize to team_a — and any two ids in a non-Latin script collapse to
// underscores — which silently interleaved two conversations in one file. Ids
// that are already clean filenames keep their exact historical path.)
func NewFileSession(dir, sessionID string) (*FileSession, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}
	name := sanitizeSessionID(sessionID)
	if name == "" {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	if name != sessionID {
		name += "-" + idFingerprint(sessionID)
	}
	path := filepath.Join(dir, name+".jsonl")
	return &FileSession{path: path, lockKey: lockKeyFor(path)}, nil
}

// idFingerprint is a short stable digest of an original session id, appended
// to a sanitized filename so lossy sanitization cannot merge two ids.
func idFingerprint(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// OpenFileSession returns a session stored at the exact file path (one
// conversation per file), creating parent directories as needed.
func OpenFileSession(path string) (*FileSession, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating session dir: %w", err)
		}
	}
	return &FileSession{path: path, lockKey: lockKeyFor(path)}, nil
}

// sanitizeSessionID maps a session ID to a safe filename component, replacing
// path separators and other unsafe characters with underscores.
func sanitizeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), ".")
}

// Entries implements agents.SessionStorage, returning entries in append order
// paginated by cursor.
func (s *FileSession) Entries(_ context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	release := acquire(s.lockKey)
	defer release()
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	// Decode everything first (skipping corrupt lines) so a bad line cannot
	// shrink the window below limit while older valid entries exist.
	entries := make([]agents.SessionEntry, 0, len(lines))
	for _, line := range lines {
		var e agents.SessionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip corrupt lines rather than failing the whole read.
			continue
		}
		entries = append(entries, e)
	}
	return agents.PageEntries(entries, cur), nil
}

// Metadata implements agents.SessionStorage.
func (s *FileSession) Metadata(_ context.Context) (agents.SessionMetadata, error) {
	release := acquire(s.lockKey)
	defer release()
	md := agents.SessionMetadata{ID: s.path}
	lines, err := s.readLines()
	if err != nil {
		return md, err
	}
	md.EntryCount = len(lines)
	if fi, serr := os.Stat(s.path); serr == nil {
		md.UpdatedAt = fi.ModTime().UTC()
	}
	return md, nil
}

// Entry implements agents.SessionStorage.
func (s *FileSession) Entry(_ context.Context, id string) (*agents.SessionEntry, error) {
	release := acquire(s.lockKey)
	defer release()
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		var e agents.SessionEntry
		if json.Unmarshal(line, &e) == nil && e.ID == id {
			return &e, nil
		}
	}
	return nil, nil
}

// Append adds entries to the session file. The batch is marshaled up
// front and written with a single write call, so a marshal failure writes
// nothing and a crash cannot interleave half-written lines from this batch.
func (s *FileSession) Append(_ context.Context, entries ...agents.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	release := acquire(s.lockKey)
	defer release()

	// Ids and parent links are assigned under the lock, from what the file
	// already holds, so two concurrent appends cannot mint the same id or link
	// to a tip that moved.
	existing, err := s.readEntries()
	if err != nil {
		return err
	}
	prepared := agents.PrepareAppend(entries, agents.AppendPointOf(existing))
	var buf bytes.Buffer
	for i := range prepared {
		data, err := json.Marshal(prepared[i])
		if err != nil {
			return fmt.Errorf("marshaling session entry: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	// Flush to disk so appends survive a crash with the same durability as the
	// rewrite path (writeLines syncs before its rename).
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReplaceEntries implements agents.EntriesReplacer: the entire history is
// swapped in one atomic file rewrite (temp file + fsync + rename), so a crash
// or write failure can never leave the session empty or half-written. An empty
// list removes the file, matching Clear.
func (s *FileSession) ReplaceEntries(_ context.Context, entries ...agents.SessionEntry) error {
	// The lock covers the read: the high-water mark below is only right if the
	// file cannot grow between reading it and the rewrite. Outside the lock, a
	// concurrent append lands after the read and the rewrite both discards its
	// entries and re-issues their sequence numbers.
	release := acquire(s.lockKey)
	defer release()

	// A replace does not restart the numbering: a cursor outlives the entries
	// it pointed at, and a history renumbered from the beginning would land
	// entirely before one and be skipped in full. What is on disk now is read
	// for its high-water mark alone.
	existing, err := s.readEntries()
	if err != nil {
		return err
	}
	prepared := agents.PrepareAppend(entries, agents.AppendPoint{LastSeq: agents.AppendPointOf(existing).LastSeq})
	lines := make([][]byte, 0, len(prepared))
	for i := range prepared {
		data, err := json.Marshal(prepared[i])
		if err != nil {
			return fmt.Errorf("marshaling session entry: %w", err)
		}
		lines = append(lines, data)
	}
	return s.writeLines(lines)
}

// PopEntry removes and returns the most recent entry, or nil if the session is
// empty. The file is rewritten atomically.
func (s *FileSession) PopEntry(_ context.Context) (*agents.SessionEntry, error) {
	return s.pop(agents.PopLast)
}

// PopItem implements agents.ItemPopper.
func (s *FileSession) PopItem(_ context.Context) (*agents.SessionEntry, error) {
	return s.pop(agents.PopLastItem)
}

// pop rewrites the file without the entry PlanPop chose, applying the relinks
// in the same rewrite — the delete and the repair are one atomic replacement,
// so the file is never on disk with a child hanging off an id that is gone.
//
// The read is STRICT, unlike Entries: a removal decided on a view with a hole
// in it takes the wrong entry, and the rewrite would then silently destroy
// every line that failed to decode — the only copy of whatever those lines
// held. Refusing is the contract (spec §2.5e2, "what must be one step"): a
// record that cannot be read cannot be part of deciding a removal.
func (s *FileSession) pop(mode agents.PopMode) (*agents.SessionEntry, error) {
	release := acquire(s.lockKey)
	defer release()
	entries, err := s.readEntriesStrict()
	if err != nil {
		return nil, err
	}
	plan, ok := agents.PlanPop(entries, mode)
	if !ok {
		return nil, nil
	}
	kept := agents.ApplyRemoval(entries, plan)
	lines := make([][]byte, 0, len(kept))
	for i := range kept {
		data, merr := json.Marshal(kept[i])
		if merr != nil {
			return nil, fmt.Errorf("marshaling session entry: %w", merr)
		}
		lines = append(lines, data)
	}
	if err := s.writeLines(lines); err != nil {
		return nil, err
	}
	return &plan.Entry, nil
}

// Clear removes all entries in the session.
func (s *FileSession) Clear(_ context.Context) error {
	release := acquire(s.lockKey)
	defer release()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readEntries decodes the file's entries, skipping lines that fail to decode —
// one bad record must not make the whole session unreadable. Callers must hold
// the per-path lock. A caller about to REWRITE the file from the result must
// use readEntriesStrict instead: writing back a lenient read destroys the
// skipped lines.
func (s *FileSession) readEntries() ([]agents.SessionEntry, error) {
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	out := make([]agents.SessionEntry, 0, len(lines))
	for _, line := range lines {
		var e agents.SessionEntry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// readEntriesStrict decodes the file's entries, failing on the first line that
// cannot be decoded. Callers must hold the per-path lock.
func (s *FileSession) readEntriesStrict() ([]agents.SessionEntry, error) {
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	out := make([]agents.SessionEntry, 0, len(lines))
	for i, line := range lines {
		var e agents.SessionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("session file %s: line %d cannot be decoded: %w", s.path, i+1, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// readLines returns the non-empty lines of the session file, or nil if it does
// not exist yet. Callers must hold the per-path lock.
func (s *FileSession) readLines() ([][]byte, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines [][]byte
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// writeLines atomically replaces the session file with the given lines,
// preserving its permissions. Callers must hold the per-path lock.
func (s *FileSession) writeLines(lines [][]byte) error {
	if len(lines) == 0 {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(s.path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".session-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		cleanup()
		return err
	}
	// Flush to disk before the rename so a crash cannot publish a truncated file.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		// Rename failed, so the temp file was never published; remove it rather
		// than leaking a .session-* file next to the session on every failure.
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

var (
	_ agents.SessionStorage = (*FileSession)(nil)
	_ agents.AtomicReplacer = (*FileSession)(nil)
	_ agents.EntryPopper    = (*FileSession)(nil)
	_ agents.ItemPopper     = (*FileSession)(nil)
)
