package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// ProjectStore persists projects — per-user working trees on a sandbox
// target (decisions §5.28).
type ProjectStore struct {
	*CrudStore[Project]
	db *bun.DB
}

// NewProjectStore returns a ProjectStore backed by db. (owner, target, name)
// uniqueness is enforced by the DB (idx_projects_owner_target_name).
func NewProjectStore(db *bun.DB) *ProjectStore {
	return &ProjectStore{CrudStore: NewCrudStore[Project](db, "project", "name ASC").withSecrets(sealProject, openProject), db: db}
}

// Create inserts the project while both rows it names still exist: the target
// and the template are locked (lockRow) for the insert's duration, so a racing
// delete of either arrives first and refuses the create — never an orphan
// project (decisions §5.28). ErrNotFound names which one was missing. The
// insert is its own transaction, bypassing the CrudStore write path that seals
// — hence sealedWrite here, or the one path that CREATES an environment would
// write it in the clear.
func (s *ProjectStore) Create(ctx context.Context, p *Project) error {
	if p.Revision == 0 {
		p.Revision = 1
	}
	if p.RuntimeGen == 0 {
		p.RuntimeGen = 1
	}
	err := sealedWrite(p, sealProject, openProject, func() error {
		return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := lockRow(ctx, tx, &SandboxTarget{}, "id = ?", p.TargetID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return fmt.Errorf("sandbox target %s: %w", p.TargetID, err)
				}
				return err
			}
			if err := lockRow(ctx, tx, &SandboxTemplate{}, "id = ?", p.TemplateID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return fmt.Errorf("sandbox template %s: %w", p.TemplateID, err)
				}
				return err
			}
			_, ierr := tx.NewInsert().Model(p).Exec(ctx)
			return ierr
		})
	})
	if err != nil {
		return fmt.Errorf("creating project: %w", err)
	}
	return nil
}

// Update overwrites the project's editable fields — name, template and
// environment — under the same compare-and-set the target uses: the write
// lands only while the row is still at expectedRevision (see
// ErrRevisionConflict). contentChanged bumps the runtime generation alongside
// the revision; a rename moves the revision alone so nothing downstream
// replaces a container or severs a terminal. Owner and target are the
// project's identity and are not writable here (decisions §5.33).
func (s *ProjectStore) Update(ctx context.Context, id string, p *Project, expectedRevision int64, contentChanged bool) error {
	p.ID = id
	genBump := 0
	if contentChanged {
		genBump = 1
	}
	var res sql.Result
	err := sealedWrite(p, sealProject, openProject, func() error {
		// The template row is locked for the write's duration, exactly as the
		// create locks it: a template delete is guarded by "no project uses
		// me", so switching a project ONTO a template racing its delete would
		// otherwise leave a project pointing at nothing.
		return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := lockRow(ctx, tx, &SandboxTemplate{}, "id = ?", p.TemplateID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return fmt.Errorf("sandbox template %s: %w", p.TemplateID, err)
				}
				return err
			}
			var uerr error
			res, uerr = tx.NewUpdate().Model(p).
				Column("name", "template_id", "env", "updated_at").
				Set("revision = revision + 1").
				Set("runtime_gen = runtime_gen + ?", genBump).
				Where("id = ?", id).
				Where("revision = ?", expectedRevision).
				Exec(ctx)
			return uerr
		})
	})
	if err != nil {
		return fmt.Errorf("updating project %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return nil
	}
	exists, eerr := s.db.NewSelect().Model((*Project)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return fmt.Errorf("updating project %s: %w", id, eerr)
	}
	if !exists {
		return fmt.Errorf("updating project %s: %w", id, ErrNotFound)
	}
	return ErrRevisionConflict
}

// ProjectGen is one project's id paired with its runtime generation after a
// bump — what the caller needs to retire that project's live instance and
// terminals, without re-reading the rows.
type ProjectGen struct {
	ID         string `bun:"id"`
	RuntimeGen int64  `bun:"runtime_gen"`
}

// BumpRuntimeGen moves the runtime generation of every project whose column
// holds id, and reports the projects it moved with their new generations. It
// is how a target or template content change reaches the containers built from
// it: the project's generation is the workbench's ONE runtime axis
// (decisions §5.33), so a change anywhere upstream shows up as a project the
// manager already knows how to retire.
//
// column is a fixed identifier chosen by the caller (target_id / template_id),
// never user input. The read is inside the write's transaction, so the
// generations reported are exactly the ones the update wrote.
func (s *ProjectStore) BumpRuntimeGen(ctx context.Context, column, id string) ([]ProjectGen, error) {
	var out []ProjectGen
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*Project)(nil)).
			Set("runtime_gen = runtime_gen + 1").
			Where("? = ?", bun.Ident(column), id).
			Exec(ctx); err != nil {
			return err
		}
		return tx.NewSelect().Model((*Project)(nil)).
			Column("id", "runtime_gen").
			Where("? = ?", bun.Ident(column), id).
			Scan(ctx, &out)
	})
	if err != nil {
		return nil, fmt.Errorf("bumping project generations for %s %s: %w", column, id, err)
	}
	return out, nil
}

// List returns one user's projects, or every owner's for EveryOwner (the
// admin listing). Each row carries its bound-session count. The two orders
// answer two questions: a person PICKS from their own, so those sort by name;
// an admin WATCHES what appears across the team, so those sort newest first.
func (s *ProjectStore) List(ctx context.Context, ownerID string) ([]Project, error) {
	var out []Project
	q := s.db.NewSelect().Model(&out).
		ColumnExpr("pj.*").
		ColumnExpr("(SELECT count(*) FROM sessions WHERE project_id = pj.id) AS session_count")
	order := "created_at DESC, id DESC"
	if ownerID != EveryOwner {
		q = q.Where("owner_id = ?", ownerID)
		order = "name ASC, id ASC"
	}
	if err := q.OrderExpr(order).Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	return out, nil
}

// DeleteIfUnreferenced deletes the project only while no session binds it —
// one atomic statement, the same race-free guard a target delete uses
// (mirrored by BindProjectIfEmpty's EXISTS on this table). It returns how
// many sessions blocked the delete; 0 with a nil error means deleted.
// Reclaiming the storage is the caller's act, once the row is gone
// (decisions §5.33).
func (s *ProjectStore) DeleteIfUnreferenced(ctx context.Context, id string) (refs int, err error) {
	res, err := s.db.NewDelete().Model((*Project)(nil)).
		Where("id = ?", id).
		Where("NOT EXISTS (SELECT 1 FROM sessions WHERE project_id = ?)", id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting project %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	exists, err := s.db.NewSelect().Model((*Project)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting project %s: %w", id, err)
	}
	if !exists {
		return 0, fmt.Errorf("deleting project %s: %w", id, ErrNotFound)
	}
	n, err := s.db.NewSelect().Model((*Session)(nil)).Where("project_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting sessions on project %s: %w", id, err)
	}
	return n, nil
}
