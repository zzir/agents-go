package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

// This file is the single home of sandbox TARGET semantics: what a payload
// must contain to be storable (NormalizeTargetConfig), when two payloads mean
// the same runtime content (TargetContentEqual), and when an update moves the
// target's identity (TargetIdentityChanged). All three decode through the same
// typed config and the same canonicalizer, so a question answered one way at
// save time cannot be answered another way at compare time.

// TargetTypes are the backends a target may name.
var TargetTypes = []string{"docker", "e2b"}

// NormalizeTargetConfig strictly decodes raw and returns the canonical payload
// to store. It gates the API write path: a payload that decodes here builds
// later — a stored type mismatch would otherwise read as its zero value at
// build time. Canonical means fields re-marshaled in struct order and unknown
// keys dropped (nothing consumes them).
func NormalizeTargetConfig(typ string, raw json.RawMessage) (json.RawMessage, error) {
	switch typ {
	case "docker":
		var dc DockerTargetConfig
		if err := unmarshalConfigJSON(raw, &dc); err != nil {
			return nil, fmt.Errorf("docker target config: %w", err)
		}
		switch {
		case dc.Host == "" || strings.HasPrefix(dc.Host, "tcp://"):
		case strings.HasPrefix(dc.Host, "ssh://"):
			if !strings.Contains(strings.TrimPrefix(dc.Host, "ssh://"), "@") {
				return nil, errors.New("an ssh:// host must carry its user: ssh://user@host")
			}
		default:
			return nil, errors.New("host must be empty (local daemon), tcp://host:port, or ssh://user@host")
		}
		return json.Marshal(dc)
	case "e2b":
		var ec E2BTargetConfig
		if err := unmarshalConfigJSON(raw, &ec); err != nil {
			return nil, fmt.Errorf("e2b target config: %w", err)
		}
		if ec.APIURL != "" && !strings.HasPrefix(ec.APIURL, "http://") && !strings.HasPrefix(ec.APIURL, "https://") {
			return nil, errors.New("api_url must be an absolute http(s) URL")
		}
		switch ec.DataPlaneAuth {
		case "", "access_token", "api_key", "none":
		default:
			return nil, errors.New(`data_plane_auth must be "", "access_token", "api_key" or "none"`)
		}
		return json.Marshal(ec)
	default:
		return nil, fmt.Errorf("sandbox target type must be docker or e2b, got %q", typ)
	}
}

// TargetContentEqual reports whether two target payloads mean the same runtime
// CONTENT — the predicate behind contentChanged (the projects' runtime-generation
// bump, and the instance retirement that follows). Canonical typed comparison keeps representation noise —
// omitted-vs-zero fields, unknown keys — from tearing down a container; a
// payload that cannot decode compares UNEQUAL, the safe side.
func TargetContentEqual(typ string, a, b json.RawMessage) bool {
	if typ == "e2b" {
		return canonicalEqual(a, b, func(*E2BTargetConfig) {})
	}
	return canonicalEqual(a, b, func(*DockerTargetConfig) {})
}

// TargetIdentityChanged reports whether an update moves the target's IDENTITY
// — the type, and the address of the machine or service a project's files live
// on; they freeze while projects live on the target (decisions §5.33). An
// undecodable prev is NOT a change — fixing it is a referenced target's only
// way out; an undecodable next counts as one, pure defense.
func TargetIdentityChanged(prev, next *SandboxTarget) bool {
	if prev.Type != next.Type {
		return true
	}
	p, perr := targetDestination(prev)
	n, nerr := targetDestination(next)
	if perr != nil {
		return false
	}
	if nerr != nil {
		return true
	}
	return p != n
}

// TargetDestinationField names the config field that IS the target's address,
// per type. The mask guard uses it: a credential sent back masked means "keep
// the stored one", which only holds while the destination is unchanged.
func TargetDestinationField(typ string) string {
	if typ == "e2b" {
		return "api_url"
	}
	return "host"
}

// targetDestination is the address itself.
func targetDestination(t *SandboxTarget) (string, error) {
	if t.Type == "e2b" {
		var ec E2BTargetConfig
		if err := unmarshalConfigJSON(t.Config, &ec); err != nil {
			return "", err
		}
		return ec.APIURL + "|" + ec.Domain, nil
	}
	var dc DockerTargetConfig
	if err := unmarshalConfigJSON(t.Config, &dc); err != nil {
		return "", err
	}
	return dc.Host, nil
}

// SandboxTargetStore persists sandbox targets.
type SandboxTargetStore struct {
	*CrudStore[SandboxTarget]
	db *bun.DB
}

// NewSandboxTargetStore returns a store backed by db.
func NewSandboxTargetStore(db *bun.DB) *SandboxTargetStore {
	return &SandboxTargetStore{
		CrudStore: NewCrudStore[SandboxTarget](db, "sandbox target", "created_at DESC").withSecrets(sealTarget, openTarget),
		db:        db,
	}
}

// Create inserts the target at revision 1 — the counter every later write
// maintains.
func (s *SandboxTargetStore) Create(ctx context.Context, t *SandboxTarget) error {
	if t.Revision == 0 {
		t.Revision = 1
	}
	return s.CrudStore.Create(ctx, t)
}

// noProjectsOnTarget is the guard the identity update and the delete share: no
// project's tree lives on this machine. It sits in the statement's WHERE
// clause, not in a prior read, so a project create landing concurrently loses
// to the database's serialization instead of slipping through a
// check-then-act window.
const noProjectsOnTarget = "NOT EXISTS (SELECT 1 FROM projects WHERE target_id = ?)"

// Update overwrites the target, shadowing the generic CrudStore update with
// the revision counter and a compare-and-set: the write lands only while the
// row is still at expectedRevision (see ErrRevisionConflict). Retiring what
// runs is the caller's next act, on the projects (ProjectStore.BumpRuntimeGen).
func (s *SandboxTargetStore) Update(ctx context.Context, id string, t *SandboxTarget, expectedRevision int64) error {
	t.ID = id
	var res sql.Result
	err := sealedWrite(t, sealTarget, openTarget, func() (err error) {
		res, err = s.db.NewUpdate().Model(t).
			ExcludeColumn("id", "created_at", "revision").
			Set("revision = revision + 1").
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("updating sandbox target %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return nil
	}
	return s.explainWriteRefusal(ctx, id)
}

// DeleteIfUnreferenced deletes the target only while no project lives on it —
// one atomic statement, closing the race where a project create lands between
// a reference count and the delete. It returns how many projects blocked the
// delete: 0 with a nil error means deleted; >0 means refused. A missing target
// is ErrNotFound — a different answer from refused, and the handler maps them
// to 404 vs 409.
//
// The old cascade (delete the target, take its unbound projects with it) is
// gone: a project delete now reclaims its storage (decisions §5.33), so a
// cascade would destroy working trees as a side effect of removing a machine.
func (s *SandboxTargetStore) DeleteIfUnreferenced(ctx context.Context, id string) (projects int, err error) {
	res, err := s.db.NewDelete().Model((*SandboxTarget)(nil)).
		Where("id = ?", id).
		Where(noProjectsOnTarget, id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting sandbox target %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	return s.countBlockers(ctx, id)
}

// UpdateIdentityIfUnreferenced overwrites the target only while no project
// lives on it AND the row is still at expectedRevision — the write path for
// updates that move the target's IDENTITY (see TargetIdentityChanged). A
// referenced target refuses with the blocking count; a moved revision is
// ErrRevisionConflict.
func (s *SandboxTargetStore) UpdateIdentityIfUnreferenced(ctx context.Context, id string, t *SandboxTarget, expectedRevision int64) (projects int, err error) {
	t.ID = id
	var res sql.Result
	err = sealedWrite(t, sealTarget, openTarget, func() (err error) {
		res, err = s.db.NewUpdate().Model(t).
			ExcludeColumn("id", "created_at", "revision").
			Set("revision = revision + 1").
			Where("id = ?", id).
			Where("revision = ?", expectedRevision).
			Where(noProjectsOnTarget, id).
			Exec(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("updating sandbox target %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	cur, gerr := s.Get(ctx, id)
	if gerr != nil {
		return 0, gerr // ErrNotFound included
	}
	if cur.Revision != expectedRevision {
		return 0, ErrRevisionConflict
	}
	return s.countBlockers(ctx, id)
}

// explainWriteRefusal disambiguates a zero-row conditional write that carried
// no reference guard: the row is missing, or its revision moved.
func (s *SandboxTargetStore) explainWriteRefusal(ctx context.Context, id string) error {
	exists, err := s.db.NewSelect().Model((*SandboxTarget)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return fmt.Errorf("checking sandbox target %s: %w", id, err)
	}
	if !exists {
		return fmt.Errorf("updating sandbox target %s: %w", id, ErrNotFound)
	}
	return ErrRevisionConflict
}

// countBlockers reports how many projects hold the target down, or ErrNotFound
// when it is gone. The reference could have vanished between the write and
// this read; it reports at least one so the caller still refuses rather than
// inventing success.
func (s *SandboxTargetStore) countBlockers(ctx context.Context, id string) (int, error) {
	exists, err := s.db.NewSelect().Model((*SandboxTarget)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return 0, fmt.Errorf("checking sandbox target %s: %w", id, err)
	}
	if !exists {
		return 0, ErrNotFound
	}
	n, err := s.db.NewSelect().Model((*Project)(nil)).Where("target_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting projects on sandbox target %s: %w", id, err)
	}
	return max(n, 1), nil
}

// canonicalEqual compares two raw payloads through T, canonicalized. The
// comparable constraint is deliberate: it keeps every field of the config
// struct a plain value, so adding a slice or map field breaks the build
// here and forces a decision about how it compares instead of silently
// changing semantics.
func canonicalEqual[T comparable](a, b json.RawMessage, canon func(*T)) bool {
	var va, vb T
	if unmarshalConfigJSON(a, &va) != nil || unmarshalConfigJSON(b, &vb) != nil {
		return false
	}
	canon(&va)
	canon(&vb)
	return va == vb
}

// unmarshalConfigJSON fills dst from raw; an absent payload is the zero
// config. Unknown keys are ignored (see TargetContentEqual for why), a type
// mismatch on a known key is an error.
func unmarshalConfigJSON[T any](raw json.RawMessage, dst *T) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}
