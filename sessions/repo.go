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

// Open returns an existing session, or an error when there is none.
//
// It checks rather than returning a handle to nothing: a typo in a session id
// would otherwise look like an empty conversation, and the run would start over
// instead of continuing.
func (r *Repo) Open(ctx context.Context, id string) (*session.Session, error) {
	var row sessionRow
	err := r.db.NewSelect().Model(&row).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("open session %q: %w", id, session.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	// Built from the row this call already read: looking the session up a
	// second time is a second chance for the id to be deleted and recreated in
	// between, which would pair this row with the replacement's storage.
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

// Delete removes a session and its entries in one transaction, so a failure
// cannot leave orphaned entries behind pointing at a session that is gone.
func (r *Repo) Delete(ctx context.Context, id string) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// The session ROW goes first, and that ordering is the fence against a
		// concurrent write: an append proves its destination exists by
		// updating this row (Session.touchIn), so once it is gone — and this
		// transaction holds its lock until commit — every concurrent append
		// either blocks and then fails, or already committed and is deleted
		// below. Deleting the entries first left a window where an append saw
		// a live row, committed, and its entries survived as orphans nothing
		// references (spec §2.5e2).
		if _, err := tx.NewDelete().Model((*sessionRow)(nil)).
			Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		// Every generation this REPO made, and only those. The direct scope —
		// an empty generation — belongs to New(db, id): a session this repo
		// never created, does not list and cannot open, so deleting it here
		// would destroy history the caller keeps somewhere else entirely.
		if _, err := tx.NewDelete().Model((*entry)(nil)).
			Where("session_id = ?", id).Where("gen <> ?", "").Exec(ctx); err != nil {
			return err
		}
		// Task rows go with the session, in both roles. A task row outlives
		// nothing: as a PARENT reference it owes a wake-up to a conversation
		// that no longer exists (retried at every restart, forever), and as a
		// CHILD reference it names a hidden transcript this delete just
		// removed. The generation columns make a surviving row inert; the
		// cascade is what stops it surviving at all.
		//
		// Deleting by id rather than by (id, gen) is deliberate here: the
		// session row is already gone, so every generation of this id is
		// unreachable — including one an older incarnation left behind.
		_, err := tx.NewDelete().Model((*taskRow)(nil)).
			Where("parent_session_id = ?", id).
			WhereOr("child_session_id = ?", id).
			Exec(ctx)
		return err
	})
}

var _ session.Repo = (*Repo)(nil)
