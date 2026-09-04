package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
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

// Create inserts the project with the sandbox row locked (lockRow) for the
// insert's duration, so a racing delete refuses the create (decisions
// §5.28); ErrNotFound when it is missing. The insert bypasses the CrudStore
// write path, hence sealedWrite here.
func (s *ProjectStore) Create(ctx context.Context, p *Project) error {
	// Assign the id before sealing: the env AAD binds to it, so it must be the
	// final id at seal time, not one the insert stamps on afterwards.
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Revision == 0 {
		p.Revision = 1
	}
	if p.RuntimeGen == 0 {
		p.RuntimeGen = 1
	}
	err := sealedWrite(p, sealProject, openProject, func() error {
		return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			sb := new(Sandbox)
			if err := lockRow(ctx, tx, sb, "id = ?", p.SandboxID); err != nil {
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

// Update overwrites the project's editable fields (name, sandbox,
// environment) under a compare-and-set on expectedRevision
// (ErrRevisionConflict); contentChanged also bumps the runtime generation. A
// move between sandboxes is allowed only to one addressing the same machine
// (ErrSandboxMoveDestination — decisions §5.36). It returns the runtime
// generation the write landed on, which the caller's retire fence must use.
func (s *ProjectStore) Update(ctx context.Context, id string, p *Project, expectedRevision int64, contentChanged bool) (int64, error) {
	p.ID = id
	genBump := 0
	if contentChanged {
		genBump = 1
	}
	var res sql.Result
	var newGen int64
	err := sealedWrite(p, sealProject, openProject, func() error {
		// The sandbox row is locked for the write's duration, as the create
		// locks it: moving ONTO a sandbox racing its delete must not land.
		return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			next := new(Sandbox)
			if err := lockRow(ctx, tx, next, "id = ?", p.SandboxID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return fmt.Errorf("sandbox %s: %w", p.SandboxID, err)
				}
				return err
			}
			if err := checkMove(ctx, tx, id, next); err != nil {
				return err
			}
			var uerr error
			res, uerr = tx.NewUpdate().Model(p).
				Column("name", "sandbox_id", "env", "updated_at").
				Set("revision = revision + 1").
				Set("runtime_gen = runtime_gen + ?", genBump).
				Where("id = ?", id).
				Where("revision = ?", expectedRevision).
				Exec(ctx)
			if uerr != nil {
				return uerr
			}
			if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
				return tx.NewSelect().Model((*Project)(nil)).
					Column("runtime_gen").Where("id = ?", id).Scan(ctx, &newGen)
			}
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("updating project %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return newGen, nil
	}
	exists, eerr := s.db.NewSelect().Model((*Project)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return 0, fmt.Errorf("updating project %s: %w", id, eerr)
	}
	if !exists {
		return 0, fmt.Errorf("updating project %s: %w", id, ErrNotFound)
	}
	return 0, ErrRevisionConflict
}

// ErrSandboxMoveDestination refuses a project move that would change the
// machine its files live on.
var ErrSandboxMoveDestination = errors.New("a project cannot move to a sandbox on another machine: its files stay where they are")

// checkMove permits a project's sandbox to change only among sandboxes that
// address the same machine; read inside the caller's transaction.
func checkMove(ctx context.Context, tx bun.Tx, projectID string, next *Sandbox) error {
	cur := new(Project)
	if err := tx.NewSelect().Model(cur).Column("sandbox_id").Where("id = ?", projectID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the CAS below answers a missing project
		}
		return err
	}
	if cur.SandboxID == next.ID {
		return nil
	}
	prev := new(Sandbox)
	if err := lockRow(ctx, tx, prev, "id = ?", cur.SandboxID); err != nil {
		return err
	}
	if prev.Type != next.Type {
		return ErrSandboxMoveDestination
	}
	pd, perr := sandboxDestination(prev)
	nd, nerr := sandboxDestination(next)
	if perr != nil || nerr != nil || pd != nd {
		return ErrSandboxMoveDestination
	}
	return nil
}

// SetInstanceRef records a backend's handle on the project's sandbox: a plain
// overwrite that moves neither counter (bumping the runtime generation would replace the instance that just reported it).
func (s *ProjectStore) SetInstanceRef(ctx context.Context, id, ref string) error {
	res, err := s.db.NewUpdate().Model((*Project)(nil)).
		Set("instance_ref = ?", ref).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("recording the sandbox for project %s: %w", id, err)
	}
	// No row means the project was deleted while its sandbox was being
	// created: report it so the backend kills the sandbox it just made.
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return fmt.Errorf("recording the sandbox for project %s: %w", id, ErrNotFound)
	}
	return nil
}

// ProjectGen is one project's id paired with its runtime generation after a
// bump — what retiring its live instance and terminals needs.
type ProjectGen struct {
	ID         string `bun:"id"`
	RuntimeGen int64  `bun:"runtime_gen"`
}

// BumpRuntimeGen moves the runtime generation of every project on the
// sandbox and reports the projects it moved with their new generations, read
// inside the write's transaction — decisions §5.33.
func (s *ProjectStore) BumpRuntimeGen(ctx context.Context, sandboxID string) ([]ProjectGen, error) {
	var out []ProjectGen
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*Project)(nil)).
			Set("runtime_gen = runtime_gen + 1").
			Where("sandbox_id = ?", sandboxID).
			Exec(ctx); err != nil {
			return err
		}
		return tx.NewSelect().Model((*Project)(nil)).
			Column("id", "runtime_gen").
			Where("sandbox_id = ?", sandboxID).
			Scan(ctx, &out)
	})
	if err != nil {
		return nil, fmt.Errorf("bumping project generations for sandbox %s: %w", sandboxID, err)
	}
	return out, nil
}

// List returns one user's projects sorted by name, or every owner's newest
// first for EveryOwner (the admin listing). Each row carries its bound-session count.
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

// DeleteIfUnreferenced deletes the project only while no session binds it:
// an in-statement NOT EXISTS on SQLite; on PostgreSQL the row is locked FOR
// UPDATE (against BindProjectIfEmpty's FOR KEY SHARE) and the guard re-read.
// Returns how many sessions blocked the delete; 0 with a nil error means
// deleted. Reclaiming the storage is the caller's act (decisions §5.33).
func (s *ProjectStore) DeleteIfUnreferenced(ctx context.Context, id string) (refs int, err error) {
	if s.db.Dialect().Name() == dialect.PG {
		err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var pid string
			lerr := tx.NewSelect().Model((*Project)(nil)).Column("id").
				Where("id = ?", id).For("UPDATE").Scan(ctx, &pid)
			if errors.Is(lerr, sql.ErrNoRows) {
				return fmt.Errorf("deleting project %s: %w", id, ErrNotFound)
			}
			if lerr != nil {
				return lerr
			}
			n, cerr := tx.NewSelect().Model((*Session)(nil)).Where("project_id = ?", id).Count(ctx)
			if cerr != nil {
				return fmt.Errorf("counting sessions on project %s: %w", id, cerr)
			}
			if n > 0 {
				refs = n
				return nil
			}
			if _, derr := tx.NewDelete().Model((*Project)(nil)).Where("id = ?", id).Exec(ctx); derr != nil {
				return fmt.Errorf("deleting project %s: %w", id, derr)
			}
			return nil
		})
		return refs, err
	}
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
