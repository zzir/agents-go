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
	entries  func(sessionID string) agents.SessionStorage
	// agentConfigID is the config a task session is bound to. It is set per
	// create by the caller through WithAgentConfig, since the SDK's
	// CreateOptions has nowhere to carry it.
	agentConfigID string
}

// NewSessionRepoAdapter wraps the session store. entries builds the storage for
// one session's history.
func NewSessionRepoAdapter(sessions *SessionStore, entries func(sessionID string) agents.SessionStorage) *SessionRepoAdapter {
	return &SessionRepoAdapter{sessions: sessions, entries: entries}
}

// WithAgentConfig returns a view that binds newly created sessions to a config.
//
// It is a copy rather than a setter because a Manager holds one repo for the
// life of the process and two concurrent spawns would otherwise race over the
// field.
func (a *SessionRepoAdapter) WithAgentConfig(id string) *SessionRepoAdapter {
	cp := *a
	cp.agentConfigID = id
	return &cp
}

// Create implements agents.SessionRepo.
func (a *SessionRepoAdapter) Create(ctx context.Context, opts agents.CreateOptions) (*agents.Session, error) {
	id := opts.ID
	if id == "" {
		id = NewID()
	}
	row := &Session{
		ID:            id,
		Name:          opts.Title,
		AgentConfigID: a.agentConfigID,
		// Hidden sessions are the task transcripts: they are excluded from the
		// chat list by the task-session filter the list query already applies.
		Hidden: opts.Hidden,
	}
	if err := a.sessions.Create(ctx, row); err != nil {
		return nil, err
	}
	return agents.NewSession(a.entries(id)), nil
}

// Open implements agents.SessionRepo. An unknown id is an error, never an empty
// session: a wrong id reading as a fresh conversation makes a run start over
// instead of continuing.
func (a *SessionRepoAdapter) Open(ctx context.Context, id string) (*agents.Session, error) {
	if _, err := a.sessions.Get(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, agents.ErrSessionNotFound
		}
		return nil, err
	}
	return agents.NewSession(a.entries(id)), nil
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

// Delete implements agents.SessionRepo.
func (a *SessionRepoAdapter) Delete(ctx context.Context, id string) error {
	return a.sessions.Delete(ctx, id)
}

var _ agents.SessionRepo = (*SessionRepoAdapter)(nil)
