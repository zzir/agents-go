package filesession

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// Repo is a directory of JSONL sessions: one file per session, plus a sidecar
// holding the metadata a file cannot express.
//
// The sidecar exists because "hidden" and "title" are properties of a session,
// not of its contents, and inferring them from the entries would mean a session
// with none has neither. It is written next to the session rather than in a
// central index so a session is one self-contained pair of files — copyable,
// deletable and greppable without a registry to keep in step.
type Repo struct {
	dir string
}

// NewRepo returns a repository backed by dir, creating it if needed.
func NewRepo(dir string) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session repo %q: %w", dir, err)
	}
	return &Repo{dir: dir}, nil
}

type sidecar struct {
	ID string `json:"id"`
	// Gen is this session's generation; see session.Ref. Empty in
	// sidecars written before the field existed, which is the direct scope's
	// value and also where those sessions' entries actually are.
	Gen       string    `json:"gen,omitempty"`
	Title     string    `json:"title,omitempty"`
	Hidden    bool      `json:"hidden,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the session last changed. The entries file's mtime
	// answers most of the time, but not always: Clear REMOVES the file, and a
	// session with no timestamp at all would move backwards in a listing — to
	// the zero time — the moment it was emptied. Zero in sidecars written
	// before the field existed.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// ref is what this sidecar's session is addressed by.
func (m sidecar) ref() session.Ref { return session.Ref{ID: m.ID, Gen: m.Gen} }

func (r *Repo) sidecarPath(id string) string {
	return filepath.Join(r.dir, sanitizeSessionID(id)+".meta.json")
}

// entriesPath is where a ref's entries live.
//
// The generation is part of the NAME, so a session and the one that replaced it
// are two files and no code path can reach the wrong one by holding an id. The
// direct scope — an empty generation — is <id>.jsonl, which is exactly what
// [New] names, and is also where a session stored before generations existed
// already is.
func (r *Repo) entriesPath(ref session.Ref) string {
	name := sanitizeSessionID(ref.ID)
	if !ref.IsDirect() {
		name += "-" + ref.Gen
	}
	return filepath.Join(r.dir, name+".jsonl")
}

// session builds the storage for a ref, decorated with the sidecar's metadata.
func (r *Repo) session(meta sidecar) (*session.Session, error) {
	store, err := NewAtPath(r.entriesPath(meta.ref()))
	if err != nil {
		return nil, err
	}
	return session.NewSession(&repoStorage{Store: store, repo: r, meta: meta}), nil
}

// repoStorage is a repo session's storage: the entries file plus the sidecar.
//
// The Store alone cannot answer Metadata — title, hidden and created-at live in
// the sidecar it knows nothing about — so handing a bare Store to a repo caller
// loses them, and the listing and the opened session then give two different
// answers about the same session. This wrapper is the one path (spec
// §2.5e2, "the change record"): Metadata merges the sidecar in, and every
// successful mutation stamps the sidecar's updated_at.
//
// The stamp is best-effort and its failure is not reported: entries and
// sidecar are two files that cannot commit together, and what is lost by
// staying quiet is listing order, while what is lost by reporting is a write
// that in fact succeeded.
type repoStorage struct {
	*Store
	repo *Repo
	// meta is the sidecar as of when the handle was BUILT. A handle is bound
	// then, not on first use: after a delete-and-recreate under the same id,
	// the sidecar on disk describes the replacement, and reading it back here
	// would leak the new session's metadata into the old one's handle. The
	// generation is the guard — a re-read is taken only when it still matches.
	meta sidecar
}

// Every write in these capabilities is overridden below. The embedded Store
// promotes its own, and a promoted write is one that never took the repo lock
// and never proved the session still exists — a capability added to Store lands
// here on its own, so this list is the reminder to follow it with an override.
var (
	_ session.Storage         = (*repoStorage)(nil)
	_ session.AtomicReplacer  = (*repoStorage)(nil)
	_ session.GuardedReplacer = (*repoStorage)(nil)
	_ session.EntryPopper     = (*repoStorage)(nil)
	_ session.ItemPopper      = (*repoStorage)(nil)
)

// lockKey names the per-session repo lock. Every mutation through a
// repoStorage handle, every metadata stamp and Delete itself serialize on it:
// unserialized, a delete raced a live handle's append and the append recreated
// the just-removed entries file as an orphan no listing reaches and no Delete
// can ever remove — and a metadata stamp's read-modify-write could resurrect a
// deleted sidecar wholesale. Distinct from the Store's own per-path lock (repo
// lock is taken first; the file lock nests inside), so the two never deadlock.
func (r *Repo) lockKey(id string) string {
	return lockKeyFor(r.sidecarPath(id)) + "\x00repo"
}

// alive verifies the sidecar still describes this handle's session, under the
// repo lock. Gone, or claimed by another generation: the session was deleted
// under the handle, and a write must refuse rather than recreate storage
// nothing references (spec §2.5e2: writing and proving the destination still
// exists are one step).
func (s *repoStorage) alive() error {
	cur, err := s.repo.readSidecar(s.meta.ID)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session %q: %w", s.meta.ID, session.ErrNotFound)
		}
		return fmt.Errorf("session %q: cannot verify it still exists: %w", s.meta.ID, err)
	}
	if cur.ID != s.meta.ID || cur.Gen != s.meta.Gen {
		return fmt.Errorf("session %q: %w (the name now belongs to another session)", s.meta.ID, session.ErrNotFound)
	}
	return nil
}

// Metadata merges the sidecar into the entries file's answer. The stored
// sidecar is re-read so a fresher updated_at shows through, but only while it
// still describes this handle's generation — after a delete-and-recreate it
// describes somebody else, and the metadata bound at build time answers
// instead.
func (s *repoStorage) Metadata(ctx context.Context) (session.Metadata, error) {
	md, err := s.Store.Metadata(ctx)
	if err != nil {
		return md, err
	}
	meta := s.meta
	if cur, merr := s.repo.readSidecar(s.meta.ID); merr == nil && cur.ID == s.meta.ID && cur.Gen == s.meta.Gen {
		meta = cur
	}
	md.ID = meta.ID
	md.Title = meta.Title
	md.Hidden = meta.Hidden
	md.CreatedAt = meta.CreatedAt
	if meta.UpdatedAt.After(md.UpdatedAt) {
		md.UpdatedAt = meta.UpdatedAt
	}
	if md.UpdatedAt.IsZero() {
		// No writes yet: the session sorts by when it was created, not by the
		// zero time.
		md.UpdatedAt = meta.CreatedAt
	}
	return md, nil
}

// touch stamps the sidecar's updated_at, best-effort. Callers hold the repo
// lock, so the read-modify-write cannot interleave with Delete and resurrect a
// removed sidecar; the generation guard keeps it off a replacement's sidecar
// regardless.
func (s *repoStorage) touch() {
	cur, err := s.repo.readSidecar(s.meta.ID)
	if err != nil || cur.ID != s.meta.ID || cur.Gen != s.meta.Gen {
		return
	}
	cur.UpdatedAt = time.Now().UTC()
	_ = s.repo.writeSidecar(cur)
}

func (s *repoStorage) Append(ctx context.Context, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	release := acquire(s.repo.lockKey(s.meta.ID))
	defer release()
	if err := s.alive(); err != nil {
		return err
	}
	if err := s.Store.Append(ctx, entries...); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *repoStorage) Clear(ctx context.Context) error {
	release := acquire(s.repo.lockKey(s.meta.ID))
	defer release()
	if err := s.alive(); err != nil {
		return err
	}
	if err := s.Store.Clear(ctx); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *repoStorage) ReplaceEntries(ctx context.Context, entries ...session.Entry) error {
	release := acquire(s.repo.lockKey(s.meta.ID))
	defer release()
	if err := s.alive(); err != nil {
		return err
	}
	if err := s.Store.ReplaceEntries(ctx, entries...); err != nil {
		return err
	}
	s.touch()
	return nil
}

func (s *repoStorage) ReplaceEntriesIf(ctx context.Context, expect int64, entries ...session.Entry) (bool, error) {
	release := acquire(s.repo.lockKey(s.meta.ID))
	defer release()
	if err := s.alive(); err != nil {
		return false, err
	}
	replaced, err := s.Store.ReplaceEntriesIf(ctx, expect, entries...)
	if err == nil && replaced {
		s.touch()
	}
	return replaced, err
}

func (s *repoStorage) PopEntry(ctx context.Context) (*session.Entry, error) {
	release := acquire(s.repo.lockKey(s.meta.ID))
	defer release()
	if err := s.alive(); err != nil {
		return nil, err
	}
	e, err := s.Store.PopEntry(ctx)
	if err == nil && e != nil {
		s.touch()
	}
	return e, err
}

func (s *repoStorage) PopItem(ctx context.Context) (*session.Entry, error) {
	release := acquire(s.repo.lockKey(s.meta.ID))
	defer release()
	if err := s.alive(); err != nil {
		return nil, err
	}
	e, err := s.Store.PopItem(ctx)
	if err == nil && e != nil {
		s.touch()
	}
	return e, err
}

// Create records a new session and returns it.
func (r *Repo) Create(_ context.Context, opts session.CreateOptions) (*session.Session, error) {
	id := opts.ID
	if id == "" {
		id = session.NewSessionID()
	}
	if strings.ContainsAny(id, `/\`) {
		// An id is a filename here, so a separator would escape the directory.
		return nil, fmt.Errorf("session id %q must not contain a path separator", id)
	}
	if sanitizeSessionID(id) == "" {
		// Everything in the id was stripped, so it has no filename to live
		// under and would collide with every other such id.
		return nil, fmt.Errorf("session id %q has no usable filename form", id)
	}
	// A generation of its own, so this session's entries are a file no handle
	// to a previous one of the same name can reach.
	gen, err := session.NewGeneration()
	if err != nil {
		return nil, err
	}
	meta := sidecar{ID: id, Gen: gen, Title: opts.Title, Hidden: opts.Hidden, CreatedAt: time.Now().UTC()}
	raw, merr := json.Marshal(meta)
	if merr != nil {
		return nil, merr
	}
	// O_EXCL, not WriteFile: the sidecar IS the claim on the id, and writing
	// over one would hand two callers the same session while both believe they
	// created it.
	f, err := os.OpenFile(r.sidecarPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			// Either the same id twice, or two ids that sanitize to one
			// filename ("team a" and "team+a" both become team_a). Both are
			// collisions; say which.
			if prev, rerr := r.readSidecar(id); rerr == nil && prev.ID != id {
				return nil, fmt.Errorf("session id %q collides with existing session %q (both map to %q)",
					id, prev.ID, sanitizeSessionID(id))
			}
			return nil, fmt.Errorf("session %q already exists", id)
		}
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	// Everything past the O_EXCL create must undo it on the way out. A sidecar
	// that exists but was never finished is a session nothing can reach: List
	// skips it, Open fails to parse it, and Create sees the file and reports
	// "already exists" — so the id is burned with no way to recover it.
	created := true
	defer func() {
		if created {
			_ = os.Remove(r.sidecarPath(id))
		}
	}()

	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	// The sidecar is the claim on the id; a crash must not leave a torn claim
	// that burns the name (see writeSidecar).
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	sess, err := r.session(meta)
	if err != nil {
		return nil, err
	}
	created = false
	return sess, nil
}

// writeSidecar replaces an EXISTING sidecar atomically (temp file + rename).
// The sidecar is the claim on the id — Create's O_EXCL open is what mints it —
// so an update must never leave it torn: a half-written sidecar is a session
// that cannot be opened and an id that cannot be re-created.
func (r *Repo) writeSidecar(meta sidecar) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	path := r.sidecarPath(meta.ID)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meta-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	// Sync before the rename: the rename can survive a crash the data did
	// not, publishing an empty or torn sidecar — and a torn sidecar burns the
	// id (List skips it, Open cannot parse it, Create sees it, Delete refuses
	// to guess what it was).
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// readSidecar returns the metadata stored under id's filename. The ID it
// carries is the ORIGINAL one, which is what makes a sanitization collision
// detectable: two different ids share a file, but only one wrote its own id.
func (r *Repo) readSidecar(id string) (sidecar, error) {
	raw, err := os.ReadFile(r.sidecarPath(id))
	if err != nil {
		return sidecar{}, err
	}
	var meta sidecar
	if err := json.Unmarshal(raw, &meta); err != nil {
		return sidecar{}, err
	}
	return meta, nil
}

// Open returns an existing session, or [session.ErrNotFound] when there is
// none. This is the one constructor in the package that requires the session to
// exist: [New] and [NewAtPath] happily hand back a store over a missing file.
func (r *Repo) Open(_ context.Context, id string) (*session.Session, error) {
	meta, err := r.readSidecar(id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("open session %q: %w", id, session.ErrNotFound)
		}
		return nil, err
	}
	// The file exists, but it may belong to a different id that sanitizes to
	// the same name. Opening it would silently serve somebody else's history.
	if meta.ID != id {
		return nil, fmt.Errorf("open session %q: %w (the name maps to session %q)",
			id, session.ErrNotFound, meta.ID)
	}
	return r.session(meta)
}

// List returns session metadata, newest first.
func (r *Repo) List(_ context.Context, opts session.ListOptions) ([]session.Metadata, error) {
	matches, err := filepath.Glob(filepath.Join(r.dir, "*.meta.json"))
	if err != nil {
		return nil, err
	}
	out := make([]session.Metadata, 0, len(matches))
	for _, path := range matches {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			// One unreadable sidecar must not make the whole listing fail;
			// the other sessions are still there and still openable.
			continue
		}
		var meta sidecar
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		if meta.Hidden && !opts.IncludeHidden {
			continue
		}
		md := session.Metadata{
			ID: meta.ID, Title: meta.Title, Hidden: meta.Hidden, CreatedAt: meta.CreatedAt,
		}
		// Last change: the entries file's mtime or the sidecar's stamp,
		// whichever is later — Clear removes the file, so the stamp is all
		// that keeps an emptied session from moving backwards. A session
		// with no writes at all sorts by when it was created.
		if fi, serr := os.Stat(r.entriesPath(meta.ref())); serr == nil {
			md.UpdatedAt = fi.ModTime().UTC()
		}
		if meta.UpdatedAt.After(md.UpdatedAt) {
			md.UpdatedAt = meta.UpdatedAt
		}
		if md.UpdatedAt.IsZero() {
			md.UpdatedAt = meta.CreatedAt
		}
		out = append(out, md)
	}
	sortByUpdatedDesc(out)
	if opts.Limit > 0 && opts.Limit < len(out) {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Delete removes a session's entries and its metadata, under the same
// per-session lock every write through a repo handle holds — deletion is a
// write like any other, and unserialized it raced appends into recreating the
// entries file it had just removed.
func (r *Repo) Delete(_ context.Context, id string) error {
	release := acquire(r.lockKey(id))
	defer release()
	meta, err := r.readSidecar(id)
	switch {
	case os.IsNotExist(err):
		// No sidecar, so this repo has no such session and there is nothing of
		// its own to remove. It must NOT fall back to the id-derived name:
		// that is the direct scope, where a plain New(dir, id) keeps its
		// history, and a repo does not delete what it never created.
		return nil
	case err != nil:
		// Not being able to read who owns the name is a reason to stop, not to
		// proceed: two ids can share a filename, and only the sidecar says
		// which of them this one is.
		return fmt.Errorf("delete session %q: cannot verify which session %q holds: %w",
			id, r.sidecarPath(id), err)
	case meta.ID != id:
		return fmt.Errorf("delete session %q: the name maps to session %q", id, meta.ID)
	}
	// The sidecar goes first: a session with entries but no sidecar is
	// invisible to List and Open, whereas the reverse would leave a session
	// that lists but cannot be read.
	if err := os.Remove(r.sidecarPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// By ref, so a delete cannot reach a generation the caller did not mean —
	// including the direct scope, which shares the id and nothing else.
	if err := os.Remove(r.entriesPath(meta.ref())); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sortByUpdatedDesc(md []session.Metadata) {
	slices.SortStableFunc(md, func(a, b session.Metadata) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
}

var _ session.Repo = (*Repo)(nil)
