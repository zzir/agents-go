package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
)

// Repo is a SQL-backed agents.SessionRepo: it owns which sessions exist,
// separately from what each one holds.
//
// Sessions used to exist only as a side effect of having entries, which meant a
// session with none was indistinguishable from one that had never been created
// — and "hide this session from listings" had nowhere to live but each caller's
// own filter.
type Repo struct {
	db *bun.DB
}

// NewRepo wraps a *bun.DB as a session repository. Call CreateSchema once
// before first use.
func NewRepo(db *bun.DB) *Repo { return &Repo{db: db} }

// Create records a new session and returns it.
func (r *Repo) Create(ctx context.Context, opts agents.CreateOptions) (*agents.Session, error) {
	id := opts.ID
	if id == "" {
		id = newSessionID()
	}
	now := time.Now().UTC()
	row := sessionRow{ID: id, Title: opts.Title, Hidden: opts.Hidden, CreatedAt: now, UpdatedAt: now}
	if _, err := r.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	return agents.NewSession(New(r.db, id)), nil
}

// Open returns an existing session, or an error when there is none.
//
// It checks rather than returning a handle to nothing: a typo in a session id
// would otherwise look like an empty conversation, and the run would start over
// instead of continuing.
func (r *Repo) Open(ctx context.Context, id string) (*agents.Session, error) {
	var row sessionRow
	err := r.db.NewSelect().Model(&row).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("open session %q: %w", id, agents.ErrSessionNotFound)
	}
	if err != nil {
		return nil, err
	}
	return agents.NewSession(New(r.db, id)), nil
}

// List returns session metadata, newest first.
func (r *Repo) List(ctx context.Context, opts agents.ListOptions) ([]agents.SessionMetadata, error) {
	q := r.db.NewSelect().Model((*sessionRow)(nil)).Order("updated_at DESC")
	if !opts.IncludeHidden {
		q = q.Where("hidden = ?", false)
	}
	if opts.Cursor.Limit > 0 {
		q = q.Limit(opts.Cursor.Limit)
	}
	var rows []sessionRow
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]agents.SessionMetadata, 0, len(rows))
	for _, row := range rows {
		out = append(out, agents.SessionMetadata{
			ID:        row.ID,
			Title:     row.Title,
			Hidden:    row.Hidden,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// Delete removes a session and its entries in one transaction, so a failure
// cannot leave orphaned entries behind pointing at a session that is gone.
func (r *Repo) Delete(ctx context.Context, id string) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*entry)(nil)).Where("session_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*sessionRow)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
}

var _ agents.SessionRepo = (*Repo)(nil)

func newSessionID() string {
	return fmt.Sprintf("sess_%d", time.Now().UnixNano())
}
