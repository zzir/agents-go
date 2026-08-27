package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// ErrTemplateTypeImmutable refuses a template type change. A template's type
// is what makes it usable on a target at all, and every project pointing at
// it was validated against the old one; rewriting it in place would leave
// those projects referencing a template their machine cannot run. Creating a
// second template is the way, and it costs nothing.
var ErrTemplateTypeImmutable = errors.New("a template's type cannot change; create a new template instead")

// NormalizeTemplateConfig strictly decodes raw and enforces the fields a
// container cannot be created without. See NormalizeTargetConfig for what
// canonical means and why the write path gates here.
func NormalizeTemplateConfig(typ string, raw json.RawMessage) (json.RawMessage, error) {
	switch typ {
	case "docker":
		var dc DockerTemplateConfig
		if err := unmarshalConfigJSON(raw, &dc); err != nil {
			return nil, fmt.Errorf("docker template config: %w", err)
		}
		if dc.Image == "" {
			return nil, errors.New("a docker template requires config.image")
		}
		if dc.MaxReadFileBytes < 0 {
			return nil, errors.New("max_read_file_bytes cannot be negative")
		}
		if dc.MemoryMB < 0 || dc.CPUs < 0 {
			return nil, errors.New("memory_mb and cpus cannot be negative")
		}
		return json.Marshal(dc)
	case "e2b":
		var ec E2BTemplateConfig
		if err := unmarshalConfigJSON(raw, &ec); err != nil {
			return nil, fmt.Errorf("e2b template config: %w", err)
		}
		if ec.TemplateID == "" {
			return nil, errors.New("an e2b template requires config.template_id — build it on the service first")
		}
		if ec.TimeoutSeconds < 0 || ec.MaxReadFileBytes < 0 {
			return nil, errors.New("timeout_seconds and max_read_file_bytes cannot be negative")
		}
		return json.Marshal(ec)
	default:
		return nil, fmt.Errorf("sandbox template type must be docker or e2b, got %q", typ)
	}
}

// TemplateContentEqual reports whether two template payloads mean the same
// runtime content — the predicate behind the RuntimeGen bump. Semantics match
// TargetContentEqual's.
func TemplateContentEqual(typ string, a, b json.RawMessage) bool {
	if typ == "e2b" {
		return canonicalEqual(a, b, func(*E2BTemplateConfig) {})
	}
	return canonicalEqual(a, b, func(*DockerTemplateConfig) {})
}

// SandboxTemplateStore persists sandbox templates. Templates carry no
// credentials, so unlike targets they need no sealing.
type SandboxTemplateStore struct {
	*CrudStore[SandboxTemplate]
	db *bun.DB
}

// NewSandboxTemplateStore returns a store backed by db.
func NewSandboxTemplateStore(db *bun.DB) *SandboxTemplateStore {
	return &SandboxTemplateStore{
		CrudStore: NewCrudStore[SandboxTemplate](db, "sandbox template", "created_at DESC"),
		db:        db,
	}
}

// Create inserts the template at revision 1.
func (s *SandboxTemplateStore) Create(ctx context.Context, t *SandboxTemplate) error {
	if t.Revision == 0 {
		t.Revision = 1
	}
	return s.CrudStore.Create(ctx, t)
}

// Update overwrites the template under the same compare-and-set targets use:
// the write lands only while the row is still at expectedRevision. Type is
// excluded from the write — see ErrTemplateTypeImmutable. Replacing every
// referencing project's container is the caller's next act
// (ProjectStore.BumpRuntimeGen).
func (s *SandboxTemplateStore) Update(ctx context.Context, id string, t *SandboxTemplate, expectedRevision int64) error {
	t.ID = id
	res, err := s.db.NewUpdate().Model(t).
		ExcludeColumn("id", "created_at", "revision", "type").
		Set("revision = revision + 1").
		Where("id = ?", id).
		Where("revision = ?", expectedRevision).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("updating sandbox template %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return nil
	}
	exists, eerr := s.db.NewSelect().Model((*SandboxTemplate)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return fmt.Errorf("updating sandbox template %s: %w", id, eerr)
	}
	if !exists {
		return fmt.Errorf("updating sandbox template %s: %w", id, ErrNotFound)
	}
	return ErrRevisionConflict
}

// DeleteIfUnreferenced deletes the template only while no project uses it —
// one atomic statement, so a project create landing concurrently loses to the
// database's serialization. It returns how many projects blocked the delete;
// 0 with a nil error means deleted.
func (s *SandboxTemplateStore) DeleteIfUnreferenced(ctx context.Context, id string) (projects int, err error) {
	var res sql.Result
	res, err = s.db.NewDelete().Model((*SandboxTemplate)(nil)).
		Where("id = ?", id).
		Where("NOT EXISTS (SELECT 1 FROM projects WHERE template_id = ?)", id).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting sandbox template %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return 0, nil
	}
	exists, eerr := s.db.NewSelect().Model((*SandboxTemplate)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return 0, fmt.Errorf("checking sandbox template %s: %w", id, eerr)
	}
	if !exists {
		return 0, ErrNotFound
	}
	n, err := s.db.NewSelect().Model((*Project)(nil)).Where("template_id = ?", id).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting projects on sandbox template %s: %w", id, err)
	}
	// The reference could have vanished between the write and this read;
	// report at least one so the caller still refuses rather than inventing
	// success.
	return max(n, 1), nil
}
