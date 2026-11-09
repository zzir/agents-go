package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// WorkspaceStore persists workspaces — per-user working trees on a sandbox
// target (decisions §5.28).
type WorkspaceStore struct {
	*CrudStore[Workspace]
	db *bun.DB
}

// NewWorkspaceStore returns a WorkspaceStore backed by db. (owner, sandbox, name)
// is unique (idx_workspaces_owner_sandbox_name); a duplicate is a UNIQUE error.
func NewWorkspaceStore(db *bun.DB) *WorkspaceStore {
	return &WorkspaceStore{CrudStore: NewCrudStore[Workspace](db, "workspace", "name ASC").withSecrets(sealWorkspace, openWorkspace), db: db}
}

// Create inserts the workspace with the sandbox row locked for the insert's
// duration (decisions §5.28); ErrNotFound when it is missing. The insert
// bypasses the CrudStore write path, hence sealedWrite.
func (s *WorkspaceStore) Create(ctx context.Context, p *Workspace) error {
	// The id is assigned before sealing: the env AAD binds to it.
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Revision == 0 {
		p.Revision = 1
	}
	if p.RuntimeGen == 0 {
		p.RuntimeGen = 1
	}
	err := sealedWrite(p, sealWorkspace, openWorkspace, func() error {
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
		return fmt.Errorf("creating workspace: %w", err)
	}
	return nil
}

// Update overwrites the workspace's editable fields under a compare-and-set on
// expectedRevision (ErrRevisionConflict); contentChanged also bumps the
// runtime generation, which is returned for the caller's retire fence. A move
// between sandboxes must keep the machine (ErrSandboxMoveDestination, decisions §5.36).
func (s *WorkspaceStore) Update(ctx context.Context, id string, p *Workspace, expectedRevision int64, contentChanged bool) (int64, error) {
	p.ID = id
	genBump := 0
	if contentChanged {
		genBump = 1
	}
	var res sql.Result
	var newGen int64
	err := sealedWrite(p, sealWorkspace, openWorkspace, func() error {
		// The sandbox row is locked for the write's duration, as Create locks it.
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
				return tx.NewSelect().Model((*Workspace)(nil)).
					Column("runtime_gen").Where("id = ?", id).Scan(ctx, &newGen)
			}
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("updating workspace %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return newGen, nil
	}
	exists, eerr := s.db.NewSelect().Model((*Workspace)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return 0, fmt.Errorf("updating workspace %s: %w", id, eerr)
	}
	if !exists {
		return 0, fmt.Errorf("updating workspace %s: %w", id, ErrNotFound)
	}
	return 0, ErrRevisionConflict
}

// ErrSandboxMoveDestination refuses a workspace move that would change the
// machine its files live on.
var ErrSandboxMoveDestination = errors.New("a workspace cannot move to a sandbox on another machine: its files stay where they are")

// checkMove permits a workspace's sandbox to change only among sandboxes that
// address the same machine; read inside the caller's transaction.
func checkMove(ctx context.Context, tx bun.Tx, workspaceID string, next *Sandbox) error {
	cur := new(Workspace)
	if err := tx.NewSelect().Model(cur).Column("sandbox_id").Where("id = ?", workspaceID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the CAS below answers a missing workspace
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

// SetInstanceRef records a backend's handle on the workspace's sandbox: a plain
// overwrite that moves neither counter.
func (s *WorkspaceStore) SetInstanceRef(ctx context.Context, id, ref string) error {
	res, err := s.db.NewUpdate().Model((*Workspace)(nil)).
		Set("instance_ref = ?", ref).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("recording the sandbox for workspace %s: %w", id, err)
	}
	// No row: the workspace was deleted mid-provision, and the backend kills the sandbox.
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return fmt.Errorf("recording the sandbox for workspace %s: %w", id, ErrNotFound)
	}
	return nil
}

// WorkspaceGen is one workspace's id paired with its runtime generation after a
// bump — what retiring its live instance and terminals needs.
type WorkspaceGen struct {
	ID         string `bun:"id"`
	RuntimeGen int64  `bun:"runtime_gen"`
}

// BumpRuntimeGen moves the runtime generation of every workspace on the
// sandbox and reports the workspaces it moved with their new generations, read
// inside the write's transaction — decisions §5.33.
func (s *WorkspaceStore) BumpRuntimeGen(ctx context.Context, sandboxID string) ([]WorkspaceGen, error) {
	var out []WorkspaceGen
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*Workspace)(nil)).
			Set("runtime_gen = runtime_gen + 1").
			Where("sandbox_id = ?", sandboxID).
			Exec(ctx); err != nil {
			return err
		}
		return tx.NewSelect().Model((*Workspace)(nil)).
			Column("id", "runtime_gen").
			Where("sandbox_id = ?", sandboxID).
			Scan(ctx, &out)
	})
	if err != nil {
		return nil, fmt.Errorf("bumping workspace generations for sandbox %s: %w", sandboxID, err)
	}
	return out, nil
}

// List returns one user's workspaces sorted by name, or every owner's newest
// first for EveryOwner (the admin listing). Each row carries its bound-session count.
func (s *WorkspaceStore) List(ctx context.Context, ownerID string) ([]Workspace, error) {
	var out []Workspace
	q := s.db.NewSelect().Model(&out).
		ColumnExpr("ws.*").
		ColumnExpr("(SELECT count(*) FROM sessions WHERE workspace_id = ws.id) AS session_count")
	order := "created_at DESC, id DESC"
	if ownerID != EveryOwner {
		q = q.Where("owner_id = ?", ownerID)
		order = "name ASC, id ASC"
	}
	if err := q.OrderExpr(order).Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	return out, nil
}

// DeleteIfUnreferenced deletes the workspace only while no session binds it (NOT
// EXISTS on SQLite; FOR UPDATE and a re-read on PostgreSQL) and returns how
// many sessions blocked it. Reclaiming the storage is the caller's act
// (decisions §5.33).
func (s *WorkspaceStore) DeleteIfUnreferenced(ctx context.Context, id string) (refs int, err error) {
	if s.db.Dialect().Name() == dialect.PG {
		err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var pid string
			lerr := tx.NewSelect().Model((*Workspace)(nil)).Column("id").
				Where("id = ?", id).For("UPDATE").Scan(ctx, &pid)
			if errors.Is(lerr, sql.ErrNoRows) {
				return fmt.Errorf("deleting workspace %s: %w", id, ErrNotFound)
			}
			if lerr != nil {
				return lerr
			}
			n, cerr := tx.NewSelect().Model((*Session)(nil)).Where("workspace_id = ?", id).Count(ctx)
			if cerr != nil {
				return fmt.Errorf("counting sessions on workspace %s: %w", id, cerr)
			}
			if n > 0 {
				refs = n
				return nil
			}
			if _, derr := tx.NewDelete().Model((*Workspace)(nil)).Where("id = ?", id).Exec(ctx); derr != nil {
				return fmt.Errorf("deleting workspace %s: %w", id, derr)
			}
			return nil
		})
		return refs, err
	}
	res, err := s.db.NewDelete().Model((*Workspace)(nil)).
		Where("id = ?", id).
		Where("NOT EXISTS (SELECT 1 FROM sessions WHERE workspace_id = ?)", id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting workspace %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	exists, err := s.db.NewSelect().Model((*Workspace)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting workspace %s: %w", id, err)
	}
	if !exists {
		return 0, fmt.Errorf("deleting workspace %s: %w", id, ErrNotFound)
	}
	n, err := s.db.NewSelect().Model((*Session)(nil)).Where("workspace_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting sessions on workspace %s: %w", id, err)
	}
	return n, nil
}
