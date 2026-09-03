package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// repoLabelOf reduces an import source to the prefix its skills' names carry:
// "owner/repo" for github.com, the URL's host otherwise, "" for workbench-authored.
func repoLabelOf(sourceRepo string) string {
	if sourceRepo == "" {
		return ""
	}
	u, err := url.Parse(sourceRepo)
	if err != nil || u.Host == "" {
		return sourceRepo
	}
	host := strings.ToLower(u.Host) // a host is case-insensitive; a label is not
	if host == "github.com" {
		if parts := strings.Split(strings.Trim(u.Path, "/"), "/"); len(parts) >= 2 {
			return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
		}
	}
	return host
}

// QualifiedName is the model-facing skill name: "<repo label>:<name>" for an
// imported skill, the bare frontmatter name for a workbench-authored one (decisions §5.31).
func (m *Skill) QualifiedName() string {
	if m.RepoLabel != "" {
		return m.RepoLabel + ":" + m.Name
	}
	return m.Name
}

// SkillStore persists SKILL.md documents.
type SkillStore struct {
	*CrudStore[Skill]
	db *bun.DB
}

// NewSkillStore returns a SkillStore backed by db. Names are unique per scope
// (partial indexes, decisions §5.29); a duplicate is a UNIQUE error.
func NewSkillStore(db *bun.DB) *SkillStore {
	return &SkillStore{CrudStore: NewCrudStore[Skill](db, "skill", "name ASC"), db: db}
}

// Update overwrites the skill in one transaction that reads the stored row
// (locked) and hands it to prepare, the shape every scoped entity uses.
func (s *SkillStore) Update(ctx context.Context, id string, m *Skill, prepare func(prev *Skill) error) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.updateFrom(ctx, tx, id, m, prepare)
	})
	if err != nil {
		return fmt.Errorf("updating skill %s: %w", id, err)
	}
	return nil
}

// ListMeta returns the skills ownerID may see in scopedListOrder, without
// their content; a document body rides only on Get/GetByNameFor.
func (s *SkillStore) ListMeta(ctx context.Context, ownerID string, admin bool) ([]Skill, error) {
	var out []Skill
	q := s.db.NewSelect().Model(&out).
		ExcludeColumn("content").
		OrderExpr(scopedListOrder)
	q = visibleTo(q, ownerID, admin)
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing skills: %w", err)
	}
	return out, nil
}

// GetByNameFor returns the skill the given MODEL-FACING name (QualifiedName)
// resolves to FOR ownerID — their own over a global one (decisions §5.29).
// ErrNotFound-wrapping error when none matches.
func (s *SkillStore) GetByNameFor(ctx context.Context, qualified, ownerID string) (*Skill, error) {
	short := qualified
	if _, after, ok := strings.CutLast(qualified, ":"); ok {
		short = after
	}
	var rows []Skill
	err := s.db.NewSelect().Model(&rows).
		Where("name = ?", short).
		Where("(scope = ? OR owner_id = ?)", ScopeGlobal, ownerID).
		// Own-over-global is a PRIVATE row winning: owning the global one does
		// not make it the shadow (see Shadows).
		OrderExpr("CASE WHEN scope = ? AND owner_id = ? THEN 0 ELSE 1 END", ScopePrivate, ownerID).
		OrderExpr("created_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting skill %q: %w", qualified, err)
	}
	for i := range rows {
		if rows[i].QualifiedName() == qualified {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("getting skill %q: %w", qualified, ErrNotFound)
}

// RepoGroup returns the scope of ownerID's group for repo (one scope by
// construction — decisions §5.31). ok=false means they hold no such group.
func (s *SkillStore) RepoGroup(ctx context.Context, repo, ownerID string) (scope string, ok bool, err error) {
	m := new(Skill)
	err = s.db.NewSelect().Model(m).Column("scope").
		Where("source_repo = ?", repo).
		Where("owner_id = ?", ownerID).
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("finding repo group %s: %w", repo, err)
	}
	return m.Scope, true, nil
}

// ErrGroupExists marks a repo-group transfer refused because the target owner
// already holds a group for that repository. Handlers map it to 409.
var ErrGroupExists = errors.New("the new owner already has this repository")

// SetRepoOwner transfers a whole repo group to newOwner (decisions §5.31),
// refused when newOwner ALREADY holds a group for the repo. Both groups are
// locked (lockedRepoGroup) in one transaction. ErrNoSuchUser when the
// account is gone; a taken name fails the whole transfer (UNIQUE -> 409).
func (s *SkillStore) SetRepoOwner(ctx context.Context, repo, ownerID, newOwner string) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		exists, err := tx.NewSelect().Model((*User)(nil)).Where("id = ?", newOwner).Exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNoSuchUser
		}
		if _, held, err := lockedRepoGroup(ctx, tx, repo, ownerID); err != nil {
			return err
		} else if !held {
			return ErrNotFound
		}
		if _, taken, err := lockedRepoGroup(ctx, tx, repo, newOwner); err != nil {
			return err
		} else if taken {
			return ErrGroupExists
		}
		res, err := tx.NewUpdate().Model((*Skill)(nil)).
			Set("owner_id = ?", newOwner).
			Set("updated_at = ?", time.Now().UTC()).
			Where("source_repo = ?", repo).
			Where("owner_id = ?", ownerID).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("transferring repo %s: %w", repo, err)
	}
	return nil
}

// SetRepoScope flips a repo group (detached rows included) in one statement,
// all or nothing (decisions §5.31). Returns how many rows flipped.
func (s *SkillStore) SetRepoScope(ctx context.Context, repo, ownerID, scope string) (int, error) {
	res, err := s.db.NewUpdate().Model((*Skill)(nil)).
		Set("scope = ?", scope).
		Set("updated_at = ?", time.Now().UTC()).
		Where("source_repo = ?", repo).
		Where("owner_id = ?", ownerID).
		Where("scope != ?", scope).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("setting repo %s scope: %w", repo, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("setting repo %s scope: %w", repo, err)
	}
	return int(n), nil
}

// ImportDoc is one fetched SKILL.md ready to land: its place in the source
// and the parsed frontmatter the row carries.
type ImportDoc struct {
	Path, SHA, Name, Description, Content string
}

// ImportOutcome names what one document did. Action is "created", "updated",
// "unchanged" or "skipped"; Reason carries the why of a skip.
type ImportOutcome struct {
	Label, Name, Action, Reason string
}

// ApplyImport lands a whole fetched import in ONE transaction against the
// group re-read under lock (decisions §5.31). wantScope/wantExisted are what
// the caller resolved before fetching; a group that changed shape since is ErrOwnershipChanged.
func (s *SkillStore) ApplyImport(ctx context.Context, repo, owner, wantScope string, wantExisted bool, docs []ImportDoc) ([]ImportOutcome, error) {
	var out []ImportOutcome
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		scope, existed, err := lockedRepoGroup(ctx, tx, repo, owner)
		if err != nil {
			return err
		}
		if existed != wantExisted || (existed && scope != wantScope) {
			return ErrOwnershipChanged
		}
		if !existed {
			scope = ScopePrivate // a fresh group lands private to its importer
		}
		out = make([]ImportOutcome, 0, len(docs))
		for _, d := range docs {
			out = append(out, applyImportDoc(ctx, tx, repo, owner, scope, d))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("importing %s: %w", repo, err)
	}
	return out, nil
}

// lockedRepoGroup reads the group's scope with its rows locked (PostgreSQL;
// SQLite's single writer serializes).
func lockedRepoGroup(ctx context.Context, tx bun.Tx, repo, owner string) (scope string, existed bool, err error) {
	m := new(Skill)
	q := tx.NewSelect().Model(m).Column("scope").
		Where("source_repo = ?", repo).
		Where("owner_id = ?", owner).
		Limit(1)
	if tx.Dialect().Name() == dialect.PG {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return m.Scope, true, nil
}

// applyImportDoc lands one document in a savepoint, so a store fault skips
// that document without aborting the transaction (as PostgreSQL otherwise would).
func applyImportDoc(ctx context.Context, tx bun.Tx, repo, owner, scope string, d ImportDoc) ImportOutcome {
	label := d.Path
	if label == "" {
		label = repo
	}
	res := ImportOutcome{Label: label, Name: d.Name}
	err := tx.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		res.Action, res.Reason, err = importDoc(ctx, tx, repo, owner, scope, d)
		return err
	})
	if err != nil {
		res.Action, res.Reason = "skipped", importWriteReason(err, d.Name)
	}
	return res
}

// importDoc creates, refreshes or leaves the row for d, answering the action
// and a skip's reason; a store fault comes back as the error instead.
func importDoc(ctx context.Context, tx bun.Tx, repo, owner, scope string, d ImportDoc) (action, reason string, err error) {
	prev := new(Skill)
	err = tx.NewSelect().Model(prev).
		Where("source_repo = ?", repo).
		Where("COALESCE(source_path, '') = ?", d.Path).
		Where("owner_id = ?", owner).
		Limit(1).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		sk := &Skill{
			Name: d.Name, Description: d.Description, Content: d.Content,
			Scope: scope, OwnerID: owner,
			SourceRepo: repo, SourcePath: d.Path, SourceSHA: d.SHA,
		}
		if _, err := tx.NewInsert().Model(sk).Exec(ctx); err != nil {
			return "", "", err
		}
		return "created", "", nil
	case err != nil:
		return "", "", err
	case prev.Detached:
		return "skipped", "edited locally (detached)", nil
	case prev.Content == d.Content:
		return "unchanged", "", nil
	}
	upd := *prev
	upd.Name, upd.Description, upd.Content, upd.SourceSHA = d.Name, d.Description, d.Content, d.SHA
	if _, err := tx.NewUpdate().Model(&upd).
		ExcludeColumn("id", "created_at").
		Where("id = ?", prev.ID).
		Exec(ctx); err != nil {
		return "", "", err
	}
	return "updated", "", nil
}

// importWriteReason words a failed document write — a name collision reads as
// itself rather than as a constraint dump.
func importWriteReason(err error, name string) string {
	if _, dup := UniqueViolation(err); dup {
		return "name " + name + " already in use"
	}
	return err.Error()
}
