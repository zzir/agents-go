// Package memory provides persistent Session implementations for the agents SDK.
//
// FileSession stores conversation history as a JSONL file (one item per line),
// with zero external dependencies. It suits single-machine, moderate-volume use;
// for high concurrency or large histories, implement agents.Session against a
// database of your choice.
package memory

import (
	"bytes"
	"context"
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
// if needed. The session ID is sanitized for use as a filename.
func NewFileSession(dir, sessionID string) (*FileSession, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}
	name := sanitizeSessionID(sessionID)
	if name == "" {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	path := filepath.Join(dir, name+".jsonl")
	return &FileSession{path: path, lockKey: lockKeyFor(path)}, nil
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

// GetItems returns stored items oldest-first. A limit <= 0 returns all items; a
// positive limit returns the most recent `limit` items (still oldest-first).
func (s *FileSession) GetItems(_ context.Context, limit int) ([]agents.TResponseInputItem, error) {
	release := acquire(s.lockKey)
	defer release()
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	// Decode everything first (skipping corrupt lines) so a bad line cannot
	// shrink the window below limit while older valid items exist.
	items := make([]agents.TResponseInputItem, 0, len(lines))
	for _, line := range lines {
		item, err := agents.UnmarshalInputItem(line)
		if err != nil {
			// Skip corrupt lines rather than failing the whole read.
			continue
		}
		items = append(items, item)
	}
	if limit > 0 && limit < len(items) {
		items = items[len(items)-limit:]
	}
	return items, nil
}

// AddItems appends items to the session file. The batch is marshaled up front
// and written with a single write call, so a marshal failure writes nothing
// and a crash cannot interleave half-written lines from this batch.
func (s *FileSession) AddItems(_ context.Context, items []agents.TResponseInputItem) error {
	if len(items) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for i := range items {
		data, err := json.Marshal(items[i])
		if err != nil {
			return fmt.Errorf("marshaling session item: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	release := acquire(s.lockKey)
	defer release()
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

// ReplaceItems implements agents.ItemsReplacer: the entire history is swapped
// in one atomic file rewrite (temp file + fsync + rename), so a crash or
// write failure can never leave the session empty or half-written. An empty
// item list removes the file, matching Clear.
func (s *FileSession) ReplaceItems(_ context.Context, items []agents.TResponseInputItem) error {
	lines := make([][]byte, 0, len(items))
	for i := range items {
		data, err := json.Marshal(items[i])
		if err != nil {
			return fmt.Errorf("marshaling session item: %w", err)
		}
		lines = append(lines, data)
	}
	release := acquire(s.lockKey)
	defer release()
	return s.writeLines(lines)
}

// PopItem removes and returns the most recent item, or nil if the session is
// empty. The file is rewritten atomically. The item is decoded before the file
// is touched, so a corrupt last line is reported without destroying it.
func (s *FileSession) PopItem(_ context.Context) (*agents.TResponseInputItem, error) {
	release := acquire(s.lockKey)
	defer release()
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	last := lines[len(lines)-1]
	item, err := agents.UnmarshalInputItem(last)
	if err != nil {
		return nil, err
	}
	if err := s.writeLines(lines[:len(lines)-1]); err != nil {
		return nil, err
	}
	return &item, nil
}

// Clear removes all items in the session.
func (s *FileSession) Clear(_ context.Context) error {
	release := acquire(s.lockKey)
	defer release()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
	return os.Rename(tmpName, s.path)
}

var (
	_ agents.Session       = (*FileSession)(nil)
	_ agents.ItemsReplacer = (*FileSession)(nil)
)
