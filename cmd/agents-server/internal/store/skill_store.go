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

// ListMeta returns every skill without its content — the index the agent
// build and the panel list read; a document body rides only on Get/GetByName.
func (s *SkillStore) ListMeta(ctx context.Context) ([]Skill, error) {
	var out []Skill
	if err := s.db.NewSelect().Model(&out).
		ExcludeColumn("content").
		OrderExpr("name ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing skills: %w", err)
	}
	return out, nil
}

// GetByName returns the skill with the given name — the read_skill tool's
// lookup. ErrNotFound-wrapping error when it doesn't exist.
func (s *SkillStore) GetByName(ctx context.Context, name string) (*Skill, error) {
	m := new(Skill)
	if err := s.db.NewSelect().Model(m).Where("name = ?", name).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting skill %q: %w", name, err)
	}
	return m, nil
}

// FindBySource returns the skill imported from (repo, path), or nil when none
// was — how a re-import matches the row it refreshes.
func (s *SkillStore) FindBySource(ctx context.Context, repo, path string) (*Skill, error) {
	m := new(Skill)
	// COALESCE: a raw-URL import stores no path, and the nullzero column
	// holds NULL where a plain = '' would never match.
	err := s.db.NewSelect().Model(m).
		Where("source_repo = ?", repo).
		Where("COALESCE(source_path, '') = ?", path).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding skill by source %s %s: %w", repo, path, err)
	}
	return m, nil
}
