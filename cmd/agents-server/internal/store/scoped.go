package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun"
)

// The visibility vocabulary of scoped configuration (decisions §5.29): providers,
// agent configs, MCP servers, skills and workflows carry a Scope deciding who
// sees the row, and an OwnerID naming its creator — permanent, kept across
// scope flips.
const (
	ScopePrivate = "private"
	ScopeGlobal  = "global"
)

// visibleTo narrows a scoped-entity SELECT to what ownerID may see: global
// rows plus their own. An admin sees everything (management needs the whole
// table; secrets stay masked regardless).
func visibleTo(q *bun.SelectQuery, ownerID string, admin bool) *bun.SelectQuery {
	if admin {
		return q
	}
	return q.Where("(scope = ? OR owner_id = ?)", ScopeGlobal, ownerID)
}

// Visible reports whether one row is readable by the caller — the point
// lookup mirror of visibleTo, so a foreign private row answers 404 exactly
// where the list omits it.
func Visible(scope, rowOwner, callerID string, admin bool) bool {
	return admin || scope == ScopeGlobal || rowOwner == callerID
}

// NormalizeScope defaults an empty scope to private — the default for every
// creator; global is an explicit act. The owner is the creator either way.
func NormalizeScope(scope string) string {
	if scope == ScopeGlobal {
		return ScopeGlobal
	}
	return ScopePrivate
}

// ListVisibleOf returns the scoped-entity rows ownerID may see, in the listing
// order decisions §5.29 promises — see orderScoped. ONLY for the owner-grouped
// scoped tables; a table without the scope/owner columns fails the query loudly.
func ListVisibleOf[T any](ctx context.Context, s *CrudStore[T], ownerID string, admin bool) ([]T, error) {
	var out []T
	q := orderScoped(visibleTo(s.db.NewSelect().Model(&out), ownerID, admin), ownerID, admin)
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing %s: %w", s.label, err)
	}
	for i := range out {
		if err := s.opened(&out[i]); err != nil {
			return nil, fmt.Errorf("listing %s: %w", s.label, err)
		}
	}
	return out, nil
}

// orderScoped applies the listing order for the four owner-grouped scoped
// entities — agent configs, providers, MCP servers, workflows (decisions
// §5.29). A member sees OTHERS' shared rows first, then their own, each group
// newest first; an admin sees the whole table newest first, ungrouped. The
// group key is owner_id, which is permanent, so neither a rename nor a scope
// flip ever reorders a row — only a transfer does. id is the final tiebreak.
func orderScoped(q *bun.SelectQuery, ownerID string, admin bool) *bun.SelectQuery {
	if admin {
		return q.OrderExpr("created_at DESC, id DESC")
	}
	return q.OrderExpr("CASE WHEN owner_id = ? THEN 1 ELSE 0 END, created_at DESC, id DESC", ownerID)
}

// scopedListOrder is the SKILLS listing order: the shared rows first, then each
// group newest first (decisions §5.29). Skills keep this global-first shape —
// their panel groups by repository and a repo flips as a whole — while the four
// owner-grouped entities above use orderScoped. The id tiebreak keeps
// same-instant rows in one order across reloads.
const scopedListOrder = `CASE WHEN scope = 'global' THEN 0 ELSE 1 END, created_at DESC, id DESC`

// ErrSameScope marks a scope flip refused because the row already holds the
// target scope — a second demote would silently re-home the row (spec
// §5.29). Handlers map it to 409.
var ErrSameScope = errors.New("the row is already in that scope")

// SetScopeOf moves one scoped row between global and private (decisions §5.29).
// The owner never changes: a demoted row returns to its author. expectOwner
// is the owner the CALLER was authorized against — carried into the WHERE, so
// a transfer landing between that check and this write refuses the flip
// (ErrOwnershipChanged) instead of moving somebody else's row. The scope
// predicate settles the same-scope check in SQL, and the unique name indexes
// decide collisions (UNIQUE violation -> 409).
func SetScopeOf[T any](ctx context.Context, s *CrudStore[T], id, scope, expectOwner string) error {
	res, err := s.db.NewUpdate().Model((*T)(nil)).
		Set("scope = ?", scope).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("scope != ?", scope).
		Where("owner_id = ?", expectOwner).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting %s %s scope: %w", s.label, id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return fmt.Errorf("setting %s %s scope: %w", s.label, id, scopeRefusal(ctx, s, id, scope, expectOwner))
	}
	return nil
}

// scopeRefusal tells apart the three reasons a conditional scope write
// matched nothing: the row is gone, it already holds the scope, or it moved
// to another owner since the caller was authorized.
func scopeRefusal[T any](ctx context.Context, s *CrudStore[T], id, scope, expectOwner string) error {
	m := new(T)
	if err := s.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	cur, owner := scopeOwnerOf(m)
	if owner != expectOwner {
		return ErrOwnershipChanged
	}
	if cur == scope {
		return ErrSameScope
	}
	return ErrOwnershipChanged
}

// scopeOwnerOf reads the scope/owner pair off any scoped model. The five
// carry the same two columns but share no interface — a method set for two
// fields would be five implementations of nothing — so this reflects them.
func scopeOwnerOf(m any) (scope, owner string) {
	v := reflect.Indirect(reflect.ValueOf(m))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return "", ""
	}
	if f := v.FieldByName("Scope"); f.IsValid() && f.Kind() == reflect.String {
		scope = f.String()
	}
	if f := v.FieldByName("OwnerID"); f.IsValid() && f.Kind() == reflect.String {
		owner = f.String()
	}
	return scope, owner
}

// ErrNoSuchUser marks an owner transfer naming an account that does not
// exist. Handlers map it to 400.
var ErrNoSuchUser = errors.New("no such user")

// ErrOwnershipChanged marks a write refused because the row's scope or owner
// moved between the caller's authorization and the write itself — a transfer
// or a scope flip landing in between. Handlers map it to 409: the caller
// re-reads and decides again, rather than editing under a permission they no
// longer hold (decisions §5.29).
var ErrOwnershipChanged = errors.New("the configuration's owner or scope changed; reload and try again")

// DeleteOwnedBy removes a row only while it still belongs to the owner the
// caller was authorized against — the delete's half of the same rule
// SetScopeOf carries. An admin passes any owner (management reaches every
// row) by naming the one they saw. ErrOwnershipChanged when it moved.
func DeleteOwnedBy[T any](ctx context.Context, s *CrudStore[T], id, expectOwner string) error {
	res, err := s.db.NewDelete().Model((*T)(nil)).
		Where("id = ?", id).
		Where("owner_id = ?", expectOwner).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting %s %s: %w", s.label, id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		exists, eerr := s.db.NewSelect().Model((*T)(nil)).Where("id = ?", id).Exists(ctx)
		if eerr != nil {
			return fmt.Errorf("deleting %s %s: %w", s.label, id, eerr)
		}
		if !exists {
			return fmt.Errorf("deleting %s %s: %w", s.label, id, ErrNotFound)
		}
		return fmt.Errorf("deleting %s %s: %w", s.label, id, ErrOwnershipChanged)
	}
	return nil
}

// SetOwnerOf transfers one scoped row to another user — management, for an
// admin. Scope is untouched; the private unique name indexes arbitrate a
// collision in the target owner's namespace (UNIQUE violation -> 409).
func SetOwnerOf[T any](ctx context.Context, s *CrudStore[T], id, ownerID string) error {
	// The scoped tables carry no FK, so the target account is checked here.
	exists, err := s.db.NewSelect().Model((*User)(nil)).Where("id = ?", ownerID).Exists(ctx)
	if err != nil {
		return fmt.Errorf("transferring %s %s: %w", s.label, id, err)
	}
	if !exists {
		return fmt.Errorf("transferring %s %s: %w", s.label, id, ErrNoSuchUser)
	}
	res, err := s.db.NewUpdate().Model((*T)(nil)).
		Set("owner_id = ?", ownerID).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("transferring %s %s: %w", s.label, id, err)
	}
	if err := requireRows(res); err != nil {
		return fmt.Errorf("transferring %s %s: %w", s.label, id, err)
	}
	return nil
}

// Shadows reports whether a row is the caller's PRIVATE shadow of a shared
// name — the own-over-global tiebreak (decisions §5.29). Owning a global row is
// not shadowing: the owner authored it, but every member reads it, so a
// private row of the same name still wins for its owner.
func Shadows(scope, rowOwner, callerID string) bool {
	return callerID != "" && scope == ScopePrivate && rowOwner == callerID
}

// RefVisible reports whether a holder row may REFERENCE the given row: a
// global holder only global rows, a private holder global rows plus its
// owner's own — decisions §5.29.
func RefVisible(refScope, refOwner, holderScope, holderOwner string) bool {
	if holderScope == ScopeGlobal {
		return refScope == ScopeGlobal
	}
	return refScope == ScopeGlobal || refOwner == holderOwner
}
