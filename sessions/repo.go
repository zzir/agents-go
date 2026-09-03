package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
)

// Repo is a SQL-backed session.Repo: it owns which sessions exist,
// separately from what each one holds.
type Repo struct {
	db *bun.DB
}

// NewRepo wraps a *bun.DB as a session repository. Call CreateSchema once
// before first use.
func NewRepo(db *bun.DB) *Repo { return &Repo{db: db} }

// Create records a new session and returns it.
func (r *Repo) Create(ctx context.Context, opts session.CreateOptions) (*session.Session, error) {
	id := opts.ID
	if id == "" {
		id = session.NewSessionID()
	}
	gen, err := session.NewGeneration()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := sessionRow{ID: id, Gen: gen, Title: opts.Title, Hidden: opts.Hidden, CreatedAt: now, UpdatedAt: now}
	if _, err := r.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	return session.NewSession(forRef(r.db, session.Ref{ID: id, Gen: gen})), nil
}

// Open returns an existing session, or an error when there is none — a typo
// in a session id must not look like an empty conversation.
func (r *Repo) Open(ctx context.Context, id string) (*session.Session, error) {
	var row sessionRow
	err := r.db.NewSelect().Model(&row).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("open session %q: %w", id, session.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	// Built from the row this call already read: a second lookup could pair this
	// row with a deleted-and-recreated session's storage.
	return session.NewSession(forRef(r.db, session.Ref{ID: id, Gen: row.Gen})), nil
}

// List returns session metadata, newest first.
func (r *Repo) List(ctx context.Context, opts session.ListOptions) ([]session.Metadata, error) {
	q := r.db.NewSelect().Model((*sessionRow)(nil)).Order("updated_at DESC")
	if !opts.IncludeHidden {
		q = q.Where("hidden = ?", false)
	}
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	var rows []sessionRow
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]session.Metadata, 0, len(rows))
	for _, row := range rows {
		out = append(out, session.Metadata{
			ID:        row.ID,
			Title:     row.Title,
			Hidden:    row.Hidden,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// Delete removes a session, its entries, its task rows and the hidden sessions
// its tasks ran in — the whole tree, in one transaction, so a failure cannot
// leave orphans pointing at a session that is gone (spec §2.13).
func (r *Repo) Delete(ctx context.Context, id string) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Breadth-first over the task tree following only LIVE edges (liveParent /
		// liveChild): a stale row may name a child now owned elsewhere. visited ends a cycle.
		queue := []string{id}
		visited := map[string]bool{id: true}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			var children []string
			if err := tx.NewSelect().Model((*taskRow)(nil)).
				Column("child_session_id").
				Where("parent_session_id = ?", cur).
				Where(liveParent).Where(liveChild).
				Scan(ctx, &children); err != nil {
				return err
			}
			for _, c := range children {
				if !visited[c] {
					visited[c] = true
					queue = append(queue, c)
				}
			}
			if err := deleteSessionRows(ctx, tx, cur); err != nil {
				return err
			}
		}
		return nil
	})
}

// deleteSessionRows removes one session's row, entries and task rows.
func deleteSessionRows(ctx context.Context, tx bun.Tx, id string) error {
	// The session ROW goes first: an append proves its destination exists by
	// updating it (touchIn), so concurrent appends fail rather than orphan (spec §2.5e2).
	if _, err := tx.NewDelete().Model((*sessionRow)(nil)).
		Where("id = ?", id).Exec(ctx); err != nil {
		return err
	}
	// Every generation this REPO made, and only those: the direct scope (an
	// empty generation) belongs to New(db, id) and is not this repo's to delete.
	if _, err := tx.NewDelete().Model((*entry)(nil)).
		Where("session_id = ?", id).Where("gen <> ?", "").Exec(ctx); err != nil {
		return err
	}
	// Task rows go with the session in both roles, by id rather than (id, gen):
	// the row is gone, so every generation of this id is unreachable.
	_, err := tx.NewDelete().Model((*taskRow)(nil)).
		Where("parent_session_id = ?", id).
		WhereOr("child_session_id = ?", id).
		Exec(ctx)
	return err
}

var _ session.Repo = (*Repo)(nil)
