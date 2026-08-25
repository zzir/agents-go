package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// ProjectStore persists projects — per-user working trees on a sandbox
// target (spec §5.28).
type ProjectStore struct {
	*CrudStore[Project]
	db *bun.DB
}

// NewProjectStore returns a ProjectStore backed by db. (owner, sandbox, name)
// uniqueness is enforced by the DB (idx_projects_owner_sandbox_name).
func NewProjectStore(db *bun.DB) *ProjectStore {
	return &ProjectStore{CrudStore: NewCrudStore[Project](db, "project", "name ASC"), db: db}
}

// Create inserts the project while its sandbox row still exists: the sandbox
// row is locked (lockRow) for the insert's duration, so a racing sandbox
// delete either cascades this row or arrives first and refuses the create —
// never an orphan project (spec §5.28). ErrNotFound names the sandbox.
func (s *ProjectStore) Create(ctx context.Context, p *Project) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockRow(ctx, tx, &SandboxConfig{}, "id = ?", p.SandboxID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("sandbox %s: %w", p.SandboxID, err)
			}
			return err
		}
		_, err := tx.NewInsert().Model(p).Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("creating project: %w", err)
	}
	return nil
}

// List returns one user's projects, or every owner's for EveryOwner (the
// admin listing). Each row carries its bound-session count.
func (s *ProjectStore) List(ctx context.Context, ownerID string) ([]Project, error) {
	var out []Project
	q := s.db.NewSelect().Model(&out).
		ColumnExpr("pj.*").
		ColumnExpr("(SELECT count(*) FROM sessions WHERE project_id = pj.id) AS session_count")
	if ownerID != EveryOwner {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.OrderExpr("name ASC, id ASC").Scan(ctx); err != nil {
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
