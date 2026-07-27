package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
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
	// Gen is this session's generation; see agents.SessionRef. Empty in
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
func (m sidecar) ref() agents.SessionRef { return agents.SessionRef{ID: m.ID, Gen: m.Gen} }

func (r *Repo) sidecarPath(id string) string {
	return filepath.Join(r.dir, sanitizeSessionID(id)+".meta.json")
}

// entriesPath is where a ref's entries live.
//
// The generation is part of the NAME, so a session and the one that replaced it
// are two files and no code path can reach the wrong one by holding an id. The
// direct scope — an empty generation — is <id>.jsonl, which is exactly what
// NewFileSession(dir, id) opens, and is also where a session stored before
// generations existed already is.
func (r *Repo) entriesPath(ref agents.SessionRef) string {
	name := sanitizeSessionID(ref.ID)
	if !ref.IsDirect() {
		name += "-" + ref.Gen
	}
	return filepath.Join(r.dir, name+".jsonl")
}

// session builds the storage for a ref, decorated with the sidecar's metadata.
func (r *Repo) session(meta sidecar) (*agents.Session, error) {
	fs, err := OpenFileSession(r.entriesPath(meta.ref()))
	if err != nil {
		return nil, err
	}
	return agents.NewSession(&repoStorage{FileSession: fs, repo: r, meta: meta}), nil
}

// repoStorage is a repo session's storage: the entries file plus the sidecar.
//
// The FileSession alone cannot answer Metadata — title, hidden and created-at
// live in the sidecar it knows nothing about — so a session opened through the
// repo used to lose them, and the listing and the opened session gave two
// different answers about the same session. This wrapper is the one path (spec
// §2.5e2, "the change record"): Metadata merges the sidecar in, and every
// successful mutation stamps the sidecar's updated_at.
//
// The stamp is best-effort and its failure is not reported: entries and
// sidecar are two files that cannot commit together, and what is lost by
// staying quiet is listing order, while what is lost by reporting is a write
// that in fact succeeded.
type repoStorage struct {
	*FileSession
	repo *Repo
	// meta is the sidecar as of when the handle was BUILT. A handle is bound
	// then, not on first use: after a delete-and-recreate under the same id,
	// the sidecar on disk describes the replacement, and reading it back here
	// would leak the new session's metadata into the old one's handle. The
	// generation is the guard — a re-read is taken only when it still matches.
	meta sidecar
}

var (
	_ agents.SessionStorage = (*repoStorage)(nil)
	_ agents.AtomicReplacer = (*repoStorage)(nil)
	_ agents.EntryPopper    = (*repoStorage)(nil)
	_ agents.ItemPopper     = (*repoStorage)(nil)
)

// Metadata merges the sidecar into the entries file's answer. The stored
// sidecar is re-read so a fresher updated_at shows through, but only while it
// still describes this handle's generation — after a delete-and-recreate it
// describes somebody else, and the metadata bound at build time answers
// instead.
func (s *repoStorage) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	md, err := s.FileSession.Metadata(ctx)
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

// touch stamps the sidecar's updated_at, best-effort. A sidecar that no longer
// describes this handle's generation is somebody else's session and is left
// alone.
func (s *repoStorage) touch() {
	cur, err := s.repo.readSidecar(s.meta.ID)
	if err != nil || cur.ID != s.meta.ID || cur.Gen != s.meta.Gen {
		return
	}
	cur.UpdatedAt = time.Now().UTC()
	_ = s.repo.writeSidecar(cur)
}

func (s *repoStorage) Append(ctx context.Context, entries ...agents.SessionEntry) error {
	err := s.FileSession.Append(ctx, entries...)
	if err == nil && len(entries) > 0 {
		s.touch()
	}
	return err
}

func (s *repoStorage) Clear(ctx context.Context) error {
	err := s.FileSession.Clear(ctx)
	if err == nil {
		s.touch()
	}
	return err
}

func (s *repoStorage) ReplaceEntries(ctx context.Context, entries ...agents.SessionEntry) error {
	err := s.FileSession.ReplaceEntries(ctx, entries...)
	if err == nil {
		s.touch()
	}
	return err
}

func (s *repoStorage) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	e, err := s.FileSession.PopEntry(ctx)
	if err == nil && e != nil {
		s.touch()
	}
	return e, err
}

func (s *repoStorage) PopItem(ctx context.Context) (*agents.SessionEntry, error) {
	e, err := s.FileSession.PopItem(ctx)
	if err == nil && e != nil {
		s.touch()
	}
	return e, err
}

// Create records a new session and returns it.
func (r *Repo) Create(_ context.Context, opts agents.CreateOptions) (*agents.Session, error) {
	id := opts.ID
	if id == "" {
		id = fmt.Sprintf("sess_%d", time.Now().UnixNano())
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
	gen, err := agents.NewGeneration()
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

// Open returns an existing session, or an error when there is none.
func (r *Repo) Open(_ context.Context, id string) (*agents.Session, error) {
	meta, err := r.readSidecar(id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("open session %q: %w", id, agents.ErrSessionNotFound)
		}
		return nil, err
	}
	// The file exists, but it may belong to a different id that sanitizes to
	// the same name. Opening it would silently serve somebody else's history.
	if meta.ID != id {
		return nil, fmt.Errorf("open session %q: %w (the name maps to session %q)",
			id, agents.ErrSessionNotFound, meta.ID)
	}
	return r.session(meta)
}

// List returns session metadata, newest first.
func (r *Repo) List(_ context.Context, opts agents.ListOptions) ([]agents.SessionMetadata, error) {
	matches, err := filepath.Glob(filepath.Join(r.dir, "*.meta.json"))
	if err != nil {
		return nil, err
	}
	out := make([]agents.SessionMetadata, 0, len(matches))
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
		md := agents.SessionMetadata{
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
	if opts.Cursor.Limit > 0 && opts.Cursor.Limit < len(out) {
		out = out[:opts.Cursor.Limit]
	}
	return out, nil
}

// Delete removes a session's entries and its metadata.
func (r *Repo) Delete(_ context.Context, id string) error {
	meta, err := r.readSidecar(id)
	switch {
	case os.IsNotExist(err):
		// No sidecar, so this repo has no such session and there is nothing of
		// its own to remove. It must NOT fall back to the id-derived name:
		// that is the direct scope, where NewFileSession(dir, id) keeps its
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

func sortByUpdatedDesc(md []agents.SessionMetadata) {
	for i := 1; i < len(md); i++ {
		for j := i; j > 0 && md[j].UpdatedAt.After(md[j-1].UpdatedAt); j-- {
			md[j], md[j-1] = md[j-1], md[j]
		}
	}
}

var _ agents.SessionRepo = (*Repo)(nil)
