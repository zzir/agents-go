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

// repoLabelOf reduces an import source to the prefix its skills' model-facing
// names carry: "owner/repo" for a github.com source, the URL's host
// otherwise, "" for a workbench-authored skill.
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
// imported skill, the bare frontmatter name for a workbench-authored one —
// the source is part of the identity, keeping two repos' same-named skills
// apart (decisions §5.31).
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

// NewSkillStore returns a SkillStore backed by db. Names are unique per
// scope (partial indexes, decisions §5.29); a duplicate surfaces as a
// UNIQUE-constraint error that handlers map to 409.
func NewSkillStore(db *bun.DB) *SkillStore {
	return &SkillStore{CrudStore: NewCrudStore[Skill](db, "skill", "name ASC"), db: db}
}

// Update overwrites the skill in one transaction that reads the stored row
// (locked) and hands it to prepare — how scope and owner survive a
// concurrent scope flip, the same shape every other scoped entity uses.
func (s *SkillStore) Update(ctx context.Context, id string, m *Skill, prepare func(prev *Skill) error) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.updateFrom(ctx, tx, id, m, prepare)
	})
	if err != nil {
		return fmt.Errorf("updating skill %s: %w", id, err)
	}
	return nil
}

// ListMeta returns the skills ownerID may see in the scoped-listing order
// (global first, each group newest first — scopedListOrder), without their
// content — the index the agent build and the
// panel list read; a document body rides only on Get/GetByNameFor.
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

// GetByNameFor returns the skill the given MODEL-FACING name resolves to FOR
// ownerID — their own over a global one sharing it (decisions §5.29), the
// read_skill tool's lookup. Imported skills answer to their qualified name
// ("owner/repo:name"), workbench-authored ones to the bare name.
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

// FindBySource returns the row of (repo, path) IN ownerID's group — the one
// this import refreshes. Nil when none: the import creates instead. The owner
// is exact, never "the caller's or a global one": a sync names the group it
// targets (decisions §5.31), so an admin syncing somebody's published repo cannot
// silently refresh their own copy of it instead.
func (s *SkillStore) FindBySource(ctx context.Context, repo, path, ownerID string) (*Skill, error) {
	m := new(Skill)
	// COALESCE: a raw-URL import stores no path, and the nullzero column
	// holds NULL where a plain = '' would never match.
	err := s.db.NewSelect().Model(m).
		Where("source_repo = ?", repo).
		Where("COALESCE(source_path, '') = ?", path).
		Where("owner_id = ?", ownerID).
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding skill by source %s %s: %w", repo, path, err)
	}
	return m, nil
}

// RepoGroup returns the scope of ownerID's group for repo — one row's scope
// answers for all of them, the group being one scope by construction (spec
// §5.31). ok=false means they hold no such group.
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

// SetRepoOwner transfers a whole repo group to newOwner — the group is the
// unit of ownership as it is of scope (decisions §5.31), so moving one row out
// would leave a group with two authors and, after a later flip, two scopes.
// Refused when newOwner ALREADY holds a group for the repo: merging two
// groups would produce exactly the mixed-scope pile the group rule exists to
// prevent, and the unique indexes cannot see it (they partition by scope).
// The check and the move share one transaction. ErrNoSuchUser when the
// account is gone; a name taken in the target namespace fails the whole
// transfer (UNIQUE -> 409).
func (s *SkillStore) SetRepoOwner(ctx context.Context, repo, ownerID, newOwner string) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		exists, err := tx.NewSelect().Model((*User)(nil)).Where("id = ?", newOwner).Exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNoSuchUser
		}
		taken, err := tx.NewSelect().Model((*Skill)(nil)).
			Where("source_repo = ?", repo).
			Where("owner_id = ?", newOwner).
			Exists(ctx)
		if err != nil {
			return err
		}
		if taken {
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

// SetRepoScope flips a repo group — every skill imported from repo and owned
// by ownerID, detached rows included — in one statement, all or nothing: a
// name taken in the target scope fails the whole flip (UNIQUE -> 409, spec
// §5.31). Returns how many rows flipped; 0 means no row was in the source
// scope.
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

// ApplyImport lands a whole fetched import in ONE transaction (decisions §5.31).
// Fetching happens first and can take minutes; every write happens here, in
// an instant, against a group re-read under lock — so a transfer, a delete or
// a scope flip that landed during the download turns the whole import into
// ErrOwnershipChanged with nothing written, instead of scattering rows across
// two owners or leaving the group half-flipped.
//
// wantScope/wantExisted are what the caller resolved the group to before
// fetching: a group that has since changed shape refuses the apply.
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

// lockedRepoGroup reads the group's scope with its rows locked, so a
// concurrent transfer or flip waits for the import to finish rather than
// interleaving with it (PostgreSQL; SQLite's single writer serializes).
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

// applyImportDoc lands one document inside the import's transaction. A store
// fault is reported as that document's skip: one bad file must not undo the
// rest of an import that has already been fetched.
func applyImportDoc(ctx context.Context, tx bun.Tx, repo, owner, scope string, d ImportDoc) ImportOutcome {
	label := d.Path
	if label == "" {
		label = repo
	}
	res := ImportOutcome{Label: label, Name: d.Name}
	prev := new(Skill)
	err := tx.NewSelect().Model(prev).
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
		if _, ierr := tx.NewInsert().Model(sk).Exec(ctx); ierr != nil {
			res.Action, res.Reason = "skipped", importWriteReason(ierr, d.Name)
			return res
		}
		res.Action = "created"
	case err != nil:
		res.Action, res.Reason = "skipped", err.Error()
	case prev.Detached:
		res.Action, res.Reason = "skipped", "edited locally (detached)"
	case prev.Content == d.Content:
		res.Action = "unchanged"
	default:
		upd := *prev
		upd.Name, upd.Description, upd.Content, upd.SourceSHA = d.Name, d.Description, d.Content, d.SHA
		if _, uerr := tx.NewUpdate().Model(&upd).
			ExcludeColumn("id", "created_at").
			Where("id = ?", prev.ID).
			Exec(ctx); uerr != nil {
			res.Action, res.Reason = "skipped", importWriteReason(uerr, d.Name)
			return res
		}
		res.Action = "updated"
	}
	return res
}

// importWriteReason words a failed document write — a name collision reads as
// itself rather than as a constraint dump.
func importWriteReason(err error, name string) string {
	if _, dup := UniqueViolation(err); dup {
		return "name " + name + " already in use"
	}
	return err.Error()
}
