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
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Hidden    bool      `json:"hidden,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *Repo) sidecarPath(id string) string {
	return filepath.Join(r.dir, sanitizeSessionID(id)+".meta.json")
}

func (r *Repo) entriesPath(id string) string {
	return filepath.Join(r.dir, sanitizeSessionID(id)+".jsonl")
}

// session builds the storage for a session id, reusing FileSession's own path
// and id sanitization rather than duplicating it — two answers to "where does
// this session live" is one too many.
func (r *Repo) session(id string) (*agents.Session, error) {
	fs, err := NewFileSession(r.dir, id)
	if err != nil {
		return nil, err
	}
	return agents.NewSession(fs), nil
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
	meta := sidecar{ID: id, Title: opts.Title, Hidden: opts.Hidden, CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(r.sidecarPath(id), raw, 0o644); err != nil {
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	return r.session(id)
}

// Open returns an existing session, or an error when there is none.
func (r *Repo) Open(_ context.Context, id string) (*agents.Session, error) {
	if _, err := os.Stat(r.sidecarPath(id)); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("open session %q: %w", id, agents.ErrSessionNotFound)
		}
		return nil, err
	}
	return r.session(id)
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
		if fi, serr := os.Stat(r.entriesPath(meta.ID)); serr == nil {
			md.UpdatedAt = fi.ModTime().UTC()
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
	// The sidecar goes first: a session with entries but no sidecar is
	// invisible to List and Open, whereas the reverse would leave a session
	// that lists but cannot be read.
	if err := os.Remove(r.sidecarPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(r.entriesPath(id)); err != nil && !os.IsNotExist(err) {
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
