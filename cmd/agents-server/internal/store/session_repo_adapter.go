package store

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/agents/session"
)

// SessionRepoAdapter presents this server's session table as an
// session.Repo, which is what agents/tasks needs to create and delete the
// hidden session a task runs in.
//
// The adapter exists rather than the server adopting the SDK's own repo because
// a session here carries fields the SDK has no notion of — the bound agent
// config, pinning, the sandbox — and the SDK only needs four operations.
type SessionRepoAdapter struct {
	sessions *SessionStore
	entries  func(ref session.Ref) session.Storage
}

// NewSessionRepoAdapter wraps the session store. entries builds the storage for
// one session's history.
func NewSessionRepoAdapter(sessions *SessionStore, entries func(ref session.Ref) session.Storage) *SessionRepoAdapter {
	return &SessionRepoAdapter{sessions: sessions, entries: entries}
}

// Create implements session.Repo.
func (a *SessionRepoAdapter) Create(ctx context.Context, opts session.CreateOptions) (*session.Session, error) {
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
	return session.NewSession(a.entries(session.Ref{ID: row.ID, Gen: row.Gen})), nil
}

// Open implements session.Repo. An unknown id is an error, never an empty
// session: a wrong id reading as a fresh conversation makes a run start over
// instead of continuing.
func (a *SessionRepoAdapter) Open(ctx context.Context, id string) (*session.Session, error) {
	row, err := a.sessions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}
	return session.NewSession(a.entries(session.Ref{ID: row.ID, Gen: row.Gen})), nil
}

// List implements session.Repo.
func (a *SessionRepoAdapter) List(ctx context.Context, opts session.ListOptions) ([]session.Metadata, error) {
	rows, err := a.sessions.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]session.Metadata, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		if r.Hidden && !opts.IncludeHidden {
			continue
		}
		out = append(out, session.Metadata{
			ID: r.ID, Title: r.Name, Hidden: r.Hidden,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	// After the filter, so a hidden session does not consume a slot the caller
	// asked for. The store already orders newest first.
	if opts.Limit > 0 && opts.Limit < len(out) {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Delete implements session.Repo, for which deleting a session that is
// not there is not an error: the caller wanted it gone, and it is. The store's
// own Delete keeps reporting ErrNotFound, which the REST endpoint's 404 needs.
func (a *SessionRepoAdapter) Delete(ctx context.Context, id string) error {
	if err := a.sessions.Delete(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

var _ session.Repo = (*SessionRepoAdapter)(nil)
