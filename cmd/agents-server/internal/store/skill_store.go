package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// SkillStore persists SKILL.md documents.
type SkillStore struct {
	*CrudStore[Skill]
	db *bun.DB
}

// NewSkillStore returns a SkillStore backed by db. Name uniqueness is
// enforced by the DB (idx_skills_name); a duplicate surfaces as a
// UNIQUE-constraint error that handlers map to 409.
func NewSkillStore(db *bun.DB) *SkillStore {
	return &SkillStore{CrudStore: NewCrudStore[Skill](db, "skill", "name ASC"), db: db}
}

// ListMeta returns the skills ownerID may see (global first, then the
// caller's own, both by creation time — the scoped-listing order, see
// ListVisibleOf), without their content — the index the agent build and the
// panel list read; a document body rides only on Get/GetByNameFor.
func (s *SkillStore) ListMeta(ctx context.Context, ownerID string, admin bool) ([]Skill, error) {
	var out []Skill
	q := s.db.NewSelect().Model(&out).
		ExcludeColumn("content").
		OrderExpr("CASE WHEN scope = ? THEN 0 ELSE 1 END", ScopeGlobal).
		OrderExpr("created_at ASC")
	q = visibleTo(q, ownerID, admin)
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing skills: %w", err)
	}
	return out, nil
}

// GetByNameFor returns the skill the given name resolves to FOR ownerID —
// their own over a global one sharing the name (spec §5.29), the read_skill
// tool's lookup. ErrNotFound-wrapping error when neither exists.
func (s *SkillStore) GetByNameFor(ctx context.Context, name, ownerID string) (*Skill, error) {
	m := new(Skill)
	err := s.db.NewSelect().Model(m).
		Where("name = ?", name).
		Where("(scope = ? OR owner_id = ?)", ScopeGlobal, ownerID).
		OrderExpr("CASE WHEN owner_id = ? THEN 0 ELSE 1 END", ownerID).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting skill %q: %w", name, err)
	}
	return m, nil
}

// FindBySource returns the skill imported from (repo, path) that this import
// may refresh: the caller's own row (an admin's import also matches a global
// one). Nil when none — the import creates instead. Scoped so two users
// importing one repo keep separate rows.
func (s *SkillStore) FindBySource(ctx context.Context, repo, path, ownerID string, admin bool) (*Skill, error) {
	m := new(Skill)
	// COALESCE: a raw-URL import stores no path, and the nullzero column
	// holds NULL where a plain = '' would never match.
	q := s.db.NewSelect().Model(m).
		Where("source_repo = ?", repo).
		Where("COALESCE(source_path, '') = ?", path)
	if admin {
		q = q.Where("(owner_id = ? OR scope = ?)", ownerID, ScopeGlobal)
	} else {
		q = q.Where("owner_id = ?", ownerID)
	}
	err := q.OrderExpr("CASE WHEN owner_id = ? THEN 0 ELSE 1 END", ownerID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding skill by source %s %s: %w", repo, path, err)
	}
	return m, nil
}
