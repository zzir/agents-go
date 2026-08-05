// Package filesession stores conversation history as JSONL files, one entry per
// line, with zero external dependencies.
//
// [Store] is a [session.Storage] backend over a single file; wrap it in
// [session.NewSession] to get the semantics layer. [Repo] is a directory of
// them, adding the title/hidden metadata a bare file cannot express. It suits
// single-machine, moderate-volume use; for high concurrency or large histories,
// implement session.Storage against a database of your choice — the sessions
// module does exactly that for SQLite and PostgreSQL.
//
// The in-memory backends are NOT here: they are session.NewInMemoryStorage and
// session.NewInMemoryRepo, in the session package itself.
package filesession

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

	"github.com/zzir/agents-go/agents/session"
)

// pathLocks shares one mutex per session file path, so independently opened
// Store instances on the same file serialize their access. Keys are absolute
// cleaned paths.
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

// Store is a [session.Storage] backed by a JSONL file: each conversation item
// is one line of JSON. One file holds one session; pass a directory plus a
// session ID to keep multiple conversations side by side. It is goroutine-safe
// within a process — including across multiple Store instances opened on the
// same path, which share a per-path lock. Cross-process access is not locked.
//
// A Store is the storage layer alone. Pass it to [session.NewSession] for the
// layer that turns entries into model input.
type Store struct {
	path    string
	lockKey string
}

// New returns a store at dir/<sessionID>.jsonl, creating dir if needed. The
// session ID is sanitized for use as a filename; an id the sanitizer had to
// alter also gets a fingerprint of the ORIGINAL id in the name, so two
// different ids can never share a file. ("team a" and "team+a" both sanitize
// to team_a — and any two ids in a non-Latin script collapse to underscores —
// which silently interleaved two conversations in one file. Ids that are
// already clean filenames keep their exact historical path.)
//
// The file itself need not exist: a store over a missing file reads as empty
// and materializes it on the first append. Use [Repo] instead if you need
// "does this session exist?" to be answerable.
func New(dir, sessionID string) (*Store, error) {
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
	return &Store{path: path, lockKey: lockKeyFor(path)}, nil
}

// idFingerprint is a short stable digest of an original session id, appended
// to a sanitized filename so lossy sanitization cannot merge two ids.
func idFingerprint(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// NewAtPath returns a store at the exact file path (one conversation per file),
// creating parent directories as needed. It is [New] without the dir-plus-id
// layout, for a caller that already knows the filename it wants.
//
// It is deliberately not called Open: like [New] it neither requires nor checks
// that the file exists, whereas [Repo.Open] reports [session.ErrNotFound] for a
// session that was never created.
func NewAtPath(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating session dir: %w", err)
		}
	}
	return &Store{path: path, lockKey: lockKeyFor(path)}, nil
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

// Entries implements session.Storage, returning entries in append order
// paginated by cursor.
func (s *Store) Entries(_ context.Context, cur session.Cursor) ([]session.Entry, error) {
	release := acquire(s.lockKey)
	defer release()
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	// Decode everything first (skipping corrupt lines) so a bad line cannot
	// shrink the window below limit while older valid entries exist.
	entries := make([]session.Entry, 0, len(lines))
	for _, line := range lines {
		var e session.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip corrupt lines rather than failing the whole read.
			continue
		}
		entries = append(entries, e)
	}
	return session.PageEntries(entries, cur), nil
}

// Metadata implements session.Storage.
func (s *Store) Metadata(_ context.Context) (session.Metadata, error) {
	release := acquire(s.lockKey)
	defer release()
	md := session.Metadata{ID: s.path}
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

// Entry implements session.Storage.
func (s *Store) Entry(_ context.Context, id string) (*session.Entry, error) {
	release := acquire(s.lockKey)
	defer release()
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		var e session.Entry
		if json.Unmarshal(line, &e) == nil && e.ID == id {
			return &e, nil
		}
	}
	return nil, nil
}

// Append adds entries to the session file. The batch is marshaled up
// front and written with a single write call, so a marshal failure writes
// nothing and a crash cannot interleave half-written lines from this batch.
func (s *Store) Append(_ context.Context, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	release := acquire(s.lockKey)
	defer release()

	// Ids and parent links are assigned under the lock, from what the file
	// already holds, so two concurrent appends cannot mint the same id or link
	// to a tip that moved.
	at, err := s.appendPointLocked()
	if err != nil {
		return err
	}
	prepared := session.PrepareAppend(entries, at)
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

// ReplaceEntries implements session.AtomicReplacer: the entire history is
// swapped in one atomic file rewrite (temp file + fsync + rename), so a crash
// or write failure can never leave the session empty or half-written. An empty
// list removes the file, matching Clear.
func (s *Store) ReplaceEntries(_ context.Context, entries ...session.Entry) error {
	// The lock covers the read: the high-water mark below is only right if the
	// file cannot grow between reading it and the rewrite. Outside the lock, a
	// concurrent append lands after the read and the rewrite both discards its
	// entries and re-issues their sequence numbers.
	release := acquire(s.lockKey)
	defer release()
	_, err := s.replaceLocked(entries, nil)
	return err
}

// ReplaceEntriesIf implements session.GuardedReplacer. The file the guard reads
// is the file the rewrite publishes: both happen under the per-path lock every
// append takes, so an append cannot slip between them.
func (s *Store) ReplaceEntriesIf(_ context.Context, expect int64, entries ...session.Entry) (bool, error) {
	release := acquire(s.lockKey)
	defer release()
	return s.replaceLocked(entries, &expect)
}

// replaceLocked rewrites the file to hold exactly entries, reporting whether it
// wrote. A non-nil expect makes the rewrite conditional on the file's highest
// sequence number still being that one. Callers hold the per-path lock.
func (s *Store) replaceLocked(entries []session.Entry, expect *int64) (bool, error) {
	// A replace does not restart the numbering: a cursor outlives the entries
	// it pointed at, and a history renumbered from the beginning would land
	// entirely before one and be skipped in full. What is on disk now is read
	// for its high-water mark alone.
	existing, err := s.readEntries()
	if err != nil {
		return false, err
	}
	lastSeq := session.AppendPointOf(existing).LastSeq
	if expect != nil && lastSeq != *expect {
		return false, nil
	}
	prepared := session.PrepareAppend(entries, session.AppendPoint{LastSeq: lastSeq})
	lines := make([][]byte, 0, len(prepared))
	for i := range prepared {
		data, err := json.Marshal(prepared[i])
		if err != nil {
			return false, fmt.Errorf("marshaling session entry: %w", err)
		}
		lines = append(lines, data)
	}
	if err := s.writeLines(lines); err != nil {
		return false, err
	}
	return true, nil
}

// PopEntry removes and returns the most recent entry, or nil if the session is
// empty. The file is rewritten atomically.
func (s *Store) PopEntry(_ context.Context) (*session.Entry, error) {
	return s.pop(session.PopLast)
}

// PopItem implements session.ItemPopper.
func (s *Store) PopItem(_ context.Context) (*session.Entry, error) {
	return s.pop(session.PopLastItem)
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
func (s *Store) pop(mode session.PopMode) (*session.Entry, error) {
	release := acquire(s.lockKey)
	defer release()
	entries, err := s.readEntriesStrict()
	if err != nil {
		return nil, err
	}
	plan, ok := session.PlanPop(entries, mode)
	if !ok {
		return nil, nil
	}
	kept := session.ApplyRemoval(entries, plan)
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
func (s *Store) Clear(_ context.Context) error {
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
func (s *Store) readEntries() ([]session.Entry, error) {
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	out := make([]session.Entry, 0, len(lines))
	for _, line := range lines {
		var e session.Entry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// readEntriesStrict decodes the file's entries, failing on the first line that
// cannot be decoded. Callers must hold the per-path lock.
func (s *Store) readEntriesStrict() ([]session.Entry, error) {
	lines, err := s.readLines()
	if err != nil {
		return nil, err
	}
	out := make([]session.Entry, 0, len(lines))
	for i, line := range lines {
		var e session.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("session file %s: line %d cannot be decoded: %w", s.path, i+1, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// appendPointLocked reports the session's append point, for linking and
// numbering an append. Callers must hold the per-path lock.
//
// Only the tail is decoded, not the whole file. That is enough because file
// order IS sequence order: PrepareAppend issues strictly increasing numbers and
// every write path appends in the order it returns them, so the newest entry
// carries the highest sequence number this session has issued, and the tip is
// either it or — when it is a leaf move — the entry it points at. Decoding the
// whole file to learn the same two facts would make every append cost a full
// read.
//
// The scan walks backwards past lines the fold over the whole log would have
// walked past too: one that does not decode at all (the read paths skip it)
// and a leaf move whose payload does not decode (LeafOf leaves the tip where
// the entry before it put it — and nothing rejects such an entry, since
// PrepareAppend tolerates the decode failure). Stopping on either would answer
// with an empty tip and start a second, detached root on the next append.
func (s *Store) appendPointLocked() (session.AppendPoint, error) {
	lines, err := s.readLines()
	if err != nil {
		return session.AppendPoint{}, err
	}
	var at session.AppendPoint
	haveSeq := false
	for i := len(lines) - 1; i >= 0; i-- {
		var e session.Entry
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		if !haveSeq {
			at.LastSeq = e.Seq
			haveSeq = true
		}
		if e.Kind == session.EntryKindLeaf {
			if _, perr := e.LeafPayload(); perr != nil {
				continue
			}
		}
		at.Leaf = session.LeafOf([]session.Entry{e})
		break
	}
	return at, nil
}

// readLines returns the non-empty lines of the session file, or nil if it does
// not exist yet. Callers must hold the per-path lock.
func (s *Store) readLines() ([][]byte, error) {
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
func (s *Store) writeLines(lines [][]byte) error {
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
	_ session.Storage         = (*Store)(nil)
	_ session.AtomicReplacer  = (*Store)(nil)
	_ session.GuardedReplacer = (*Store)(nil)
	_ session.EntryPopper     = (*Store)(nil)
	_ session.ItemPopper      = (*Store)(nil)
)
