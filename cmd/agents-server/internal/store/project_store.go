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

// NewProjectStore returns a ProjectStore backed by db. (owner, sandbox, name)
// uniqueness is enforced by the DB (idx_projects_owner_sandbox_name).
func NewProjectStore(db *bun.DB) *ProjectStore {
	return &ProjectStore{CrudStore: NewCrudStore[Project](db, "project", "name ASC").withSecrets(sealProject, openProject), db: db}
}

// Create inserts the project while its sandbox row still exists: the sandbox
// row is locked (lockRow) for the insert's duration, so a racing sandbox
// delete either cascades this row or arrives first and refuses the create —
// never an orphan project (decisions §5.28). ErrNotFound names the sandbox.
// The insert is its own transaction, bypassing the CrudStore write path that
// seals — hence sealedWrite here, or the one path that CREATES an environment
// would write it in the clear.
func (s *ProjectStore) Create(ctx context.Context, p *Project) error {
	if p.Revision == 0 {
		p.Revision = 1
	}
	if p.RuntimeGen == 0 {
		p.RuntimeGen = 1
	}
	err := sealedWrite(p, sealProject, openProject, func() error {
		return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := lockRow(ctx, tx, &SandboxConfig{}, "id = ?", p.SandboxID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return fmt.Errorf("sandbox %s: %w", p.SandboxID, err)
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

// Update overwrites the project's editable fields — name and environment —
// under the same compare-and-set the sandbox config uses: the write lands
// only while the row is still at expectedRevision (see ErrRevisionConflict).
// contentChanged bumps the runtime generation alongside the revision; a
// rename moves the revision alone so nothing downstream replaces a container
// or severs a terminal. Owner and sandbox are the project's identity and are
// not writable here.
func (s *ProjectStore) Update(ctx context.Context, id string, p *Project, expectedRevision int64, contentChanged bool) error {
	p.ID = id
	genBump := 0
	if contentChanged {
		genBump = 1
	}
	var res sql.Result
	err := sealedWrite(p, sealProject, openProject, func() (err error) {
		res, err = s.db.NewUpdate().Model(p).
			Column("name", "env", "updated_at").
			Set("revision = revision + 1").
			Set("runtime_gen = runtime_gen + ?", genBump).
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Exec(ctx)
		return err
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

// EnsureDefault returns the owner's default project on the sandbox, creating
// it on first use. The insert race resolves through the unique name index:
// the loser re-reads the winner's row.
func (s *ProjectStore) EnsureDefault(ctx context.Context, ownerID, sandboxID string) (*Project, error) {
	get := func() (*Project, error) {
		p := new(Project)
		err := s.db.NewSelect().Model(p).
			Where("owner_id = ?", ownerID).
			Where("sandbox_id = ?", sandboxID).
			Where("name = ?", DefaultProjectName).
			Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("finding default project: %w", err)
		}
		return p, nil
	}
	if p, err := get(); err != nil || p != nil {
		return p, err
	}
	p := &Project{OwnerID: ownerID, SandboxID: sandboxID, Name: DefaultProjectName}
	err := s.Create(ctx, p)
	if err == nil {
		return p, nil
	}
	if _, dup := UniqueViolation(err); dup {
		if won, gerr := get(); gerr == nil && won != nil {
			return won, nil
		}
	}
	return nil, err
}

// DeleteIfUnreferenced deletes the project only while no session binds it —
// one atomic statement, the same race-free guard a sandbox delete uses
// (mirrored by BindSandboxIfEmpty's EXISTS on this table). It returns how
// many sessions blocked the delete; 0 with a nil error means deleted. The
// project's storage (host directory or remote volume) is NOT touched — data
// outlives the row on purpose.
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
