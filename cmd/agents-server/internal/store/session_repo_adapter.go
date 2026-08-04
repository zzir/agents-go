package store

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/agents"
)

// SessionRepoAdapter presents this server's session table as an
// agents.SessionRepo, which is what agents/tasks needs to create and delete the
// hidden session a task runs in.
//
// The adapter exists rather than the server adopting the SDK's own repo because
// a session here carries fields the SDK has no notion of — the bound agent
// config, pinning, the sandbox — and the SDK only needs four operations.
type SessionRepoAdapter struct {
	sessions *SessionStore
	entries  func(ref agents.SessionRef) agents.SessionStorage
}

// NewSessionRepoAdapter wraps the session store. entries builds the storage for
// one session's history.
func NewSessionRepoAdapter(sessions *SessionStore, entries func(ref agents.SessionRef) agents.SessionStorage) *SessionRepoAdapter {
	return &SessionRepoAdapter{sessions: sessions, entries: entries}
}

// Create implements agents.SessionRepo.
func (a *SessionRepoAdapter) Create(ctx context.Context, opts agents.CreateOptions) (*agents.Session, error) {
	id := opts.ID
	if id == "" {
		id = NewID()
	}
	row := &Session{
		ID:   id,
		Name: opts.Title,
		// Hidden sessions are the task transcripts: they are excluded from the
		// chat list by the task-session filter the list query already applies.
		Hidden: opts.Hidden,
	}
	if err := a.sessions.Create(ctx, row); err != nil {
		return nil, err
	}
	// From the row this call just wrote: resolving the id again is a second
	// chance for it to be deleted and recreated in between.
	return agents.NewSession(a.entries(agents.SessionRef{ID: row.ID, Gen: row.Gen})), nil
}

// Open implements agents.SessionRepo. An unknown id is an error, never an empty
// session: a wrong id reading as a fresh conversation makes a run start over
// instead of continuing.
func (a *SessionRepoAdapter) Open(ctx context.Context, id string) (*agents.Session, error) {
	row, err := a.sessions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, agents.ErrSessionNotFound
		}
		return nil, err
	}
	return agents.NewSession(a.entries(agents.SessionRef{ID: row.ID, Gen: row.Gen})), nil
}

// List implements agents.SessionRepo.
func (a *SessionRepoAdapter) List(ctx context.Context, opts agents.ListOptions) ([]agents.SessionMetadata, error) {
	rows, err := a.sessions.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agents.SessionMetadata, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		if r.Hidden && !opts.IncludeHidden {
			continue
		}
		out = append(out, agents.SessionMetadata{
			ID: r.ID, Title: r.Name, Hidden: r.Hidden,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// Delete implements agents.SessionRepo, for which deleting a session that is
// not there is not an error: the caller wanted it gone, and it is. The store's
// own Delete keeps reporting ErrNotFound, which the REST endpoint's 404 needs.
func (a *SessionRepoAdapter) Delete(ctx context.Context, id string) error {
	if err := a.sessions.Delete(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

var _ agents.SessionRepo = (*SessionRepoAdapter)(nil)
