package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/agents/session"
)

// SessionRepoAdapter presents this server's session table as a session.Repo,
// which is what agents/tasks needs for the hidden session a task runs in.
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
	// A served session (a task's transcript) inherits its parent's owner; one
	// without a parent (the conformance suite, tooling) belongs to the local account.
	owner := LocalUserID
	if opts.ParentID != "" {
		parent, err := a.sessions.Get(ctx, opts.ParentID)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("creating a served session: parent %s: %w", opts.ParentID, session.ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("creating a served session: parent: %w", err)
		}
		owner = parent.OwnerID
	}
	row := &Session{
		ID:      id,
		OwnerID: owner,
		Name:    opts.Title,
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
// session.
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

// List implements session.Repo: the SDK's view has no owner, so it is every
// owner's listing.
func (a *SessionRepoAdapter) List(ctx context.Context, opts session.ListOptions) ([]session.Metadata, error) {
	rows, err := a.sessions.List(ctx, EveryOwner)
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

// Delete implements session.Repo, for which deleting an absent session is
// not an error (the store's own Delete keeps reporting ErrNotFound).
func (a *SessionRepoAdapter) Delete(ctx context.Context, id string) error {
	if err := a.sessions.Delete(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

var _ session.Repo = (*SessionRepoAdapter)(nil)
