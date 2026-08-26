package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// ErrRevisionConflict reports an update whose expected revision no longer
// matches the row: another update landed between the caller's read and its
// write. Proceeding would silently overwrite that update (a credential
// rotation above all) or bypass the identity freeze through a stale identity
// comparison — the caller re-reads and retries instead. Handlers map it to
// 409.
var ErrRevisionConflict = errors.New("sandbox config changed concurrently; re-read and retry")

// SandboxStore persists sandbox configs.
type SandboxStore struct {
	*CrudStore[SandboxConfig]
}

// NewSandboxStore returns a SandboxStore backed by db.
func NewSandboxStore(db *bun.DB) *SandboxStore {
	return &SandboxStore{NewCrudStore[SandboxConfig](db, "sandbox config", "created_at DESC").withSecrets(sealSandbox, openSandbox)}
}

// Create inserts the config at revision 1 and runtime generation 1 — the two
// counters every later write maintains (see the SandboxConfig fields for what
// each fences).
func (s *SandboxStore) Create(ctx context.Context, cfg *SandboxConfig) error {
	if cfg.Revision == 0 {
		cfg.Revision = 1
	}
	if cfg.RuntimeGen == 0 {
		cfg.RuntimeGen = 1
	}
	return s.CrudStore.Create(ctx, cfg)
}

// unreferenced is the guard the identity update and the delete share: no
// session is permanently bound to this config. It lives in the statement's
// WHERE clause, not in a prior read, so a bind landing concurrently loses to
// the database's serialization instead of slipping through a check-then-act
// window.
const unreferenced = "NOT EXISTS (SELECT 1 FROM sessions WHERE sandbox_id = ?)"

// noProjects is the identity update's second guard: a project row pins the
// sandbox's identity even before any session binds — its tree (a terminal
// may already have written files) lives on this daemon (decisions §5.28). The
// delete does NOT share it: deleting a sandbox cascades its project rows.
const noProjects = "NOT EXISTS (SELECT 1 FROM projects WHERE sandbox_id = ?)"

// Update overwrites the config, shadowing the generic CrudStore update with
// the two counters and a compare-and-set: the write lands only while the row
// is still at expectedRevision — the revision the caller read, compared its
// identity against, and decided contentChanged on (see ErrRevisionConflict).
// contentChanged bumps the runtime generation alongside the revision; a
// name-only write moves the revision alone, so nothing downstream retires
// instances or severs terminals over a rename.
func (s *SandboxStore) Update(ctx context.Context, id string, cfg *SandboxConfig, expectedRevision int64, contentChanged bool) error {
	cfg.ID = id
	genBump := 0
	if contentChanged {
		genBump = 1
	}
	var res sql.Result
	err := sealedWrite(cfg, sealSandbox, openSandbox, func() (err error) {
		res, err = s.db.NewUpdate().Model(cfg).
			ExcludeColumn("id", "created_at", "revision", "runtime_gen").
			Set("revision = revision + 1").
			Set("runtime_gen = runtime_gen + ?", genBump).
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("updating sandbox config %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return nil
	}
	exists, eerr := s.db.NewSelect().Model((*SandboxConfig)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return fmt.Errorf("updating sandbox config %s: %w", id, eerr)
	}
	if !exists {
		return fmt.Errorf("updating sandbox config %s: %w", id, ErrNotFound)
	}
	return ErrRevisionConflict
}

// DeleteIfUnreferenced deletes the config only while no session is bound to
// it — one atomic statement, closing the race where a first-run bind lands
// between a reference count and the delete (which would leave the session
// permanently pointing at nothing). It returns how many sessions blocked the
// delete: 0 with a nil error means deleted; >0 means refused. A missing
// config is ErrNotFound — a different answer from refused, and the handler
// maps them to 404 vs 409.
func (s *SandboxStore) DeleteIfUnreferenced(ctx context.Context, id string) (refs int, err error) {
	var n int64
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, derr := tx.NewDelete().Model((*SandboxConfig)(nil)).
			Where("id = ?", id).
			Where(unreferenced, id).
			Exec(ctx)
		if derr != nil {
			return derr
		}
		if n, derr = res.RowsAffected(); derr != nil || n == 0 {
			return derr
		}
		// The sandbox's projects die with it: only UNBOUND ones can remain (a
		// bound session blocks the delete above), and a project row without
		// its sandbox could never build. Storage is left alone, as always.
		_, derr = tx.NewDelete().Model((*Project)(nil)).Where("sandbox_id = ?", id).Exec(ctx)
		return derr
	})
	if err != nil {
		return 0, fmt.Errorf("deleting sandbox config %s: %w", id, err)
	}
	if n > 0 {
		return 0, nil
	}
	return s.explainRefusal(ctx, id)
}

// UpdateIdentityIfUnreferenced overwrites the config only while no session
// is bound to it, no project row lives on it, AND the row is still at
// expectedRevision — the write path for updates that move the sandbox's
// IDENTITY (see IdentityChanged). A referenced config refuses with the
// blocking counts; a moved revision is ErrRevisionConflict. An identity
// change is by definition a content change, so the runtime generation bumps
// unconditionally here.
func (s *SandboxStore) UpdateIdentityIfUnreferenced(ctx context.Context, id string, cfg *SandboxConfig, expectedRevision int64) (sessions, projects int, err error) {
	cfg.ID = id
	var res sql.Result
	err = sealedWrite(cfg, sealSandbox, openSandbox, func() (err error) {
		res, err = s.db.NewUpdate().Model(cfg).
			ExcludeColumn("id", "created_at", "revision", "runtime_gen").
			Set("revision = revision + 1").
			Set("runtime_gen = runtime_gen + 1").
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Where(unreferenced, id).
			Where(noProjects, id).
			Exec(ctx)
		return err
	})
	if err != nil {
		return 0, 0, fmt.Errorf("updating sandbox config %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, 0, nil
	}
	return s.explainIdentityRefusal(ctx, id, expectedRevision)
}

// explainRefusal disambiguates a zero-row conditional write: the config is
// missing (ErrNotFound) or sessions reference it (their count, so the refusal
// can say how many).
func (s *SandboxStore) explainRefusal(ctx context.Context, id string) (int, error) {
	exists, err := s.db.NewSelect().Model((*SandboxConfig)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return 0, fmt.Errorf("checking sandbox config %s: %w", id, err)
	}
	if !exists {
		return 0, ErrNotFound
	}
	n, err := s.db.NewSelect().Model((*Session)(nil)).Where("sandbox_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting sessions bound to sandbox %s: %w", id, err)
	}
	// The reference could have vanished between the write and this read; report
	// at least one so the caller still refuses rather than inventing success.
	return max(n, 1), nil
}

// explainIdentityRefusal adds the revision dimension: missing, moved revision
// (conflict), or referenced — three different answers for three different
// remedies. Referenced reports sessions and projects separately, so the
// refusal can name what to remove.
func (s *SandboxStore) explainIdentityRefusal(ctx context.Context, id string, expectedRevision int64) (sessions, projects int, err error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return 0, 0, err // ErrNotFound included
	}
	if cur.Revision != expectedRevision {
		return 0, 0, ErrRevisionConflict
	}
	sessions, err = s.db.NewSelect().Model((*Session)(nil)).Where("sandbox_id = ?", id).Count(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("counting sessions bound to sandbox %s: %w", id, err)
	}
	projects, err = s.db.NewSelect().Model((*Project)(nil)).Where("sandbox_id = ?", id).Count(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("counting projects on sandbox %s: %w", id, err)
	}
	if sessions == 0 && projects == 0 {
		// The reference could have vanished between the write and this read;
		// report at least one so the caller still refuses rather than
		// inventing success.
		sessions = 1
	}
	return sessions, projects, nil
}
