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

// The visibility vocabulary of scoped configuration (decisions §5.29): a
// Scope deciding who sees the row, and a permanent OwnerID naming its creator.
const (
	ScopePrivate = "private"
	ScopeGlobal  = "global"
)

// visibleTo narrows a scoped-entity SELECT to what ownerID may see: global
// rows plus their own; an admin sees everything.
func visibleTo(q *bun.SelectQuery, ownerID string, admin bool) *bun.SelectQuery {
	if admin {
		return q
	}
	return q.Where("(scope = ? OR owner_id = ?)", ScopeGlobal, ownerID)
}

// Visible reports whether one row is readable by the caller — the point
// lookup mirror of visibleTo.
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

// ListVisibleOf returns the scoped-entity rows ownerID may see, in orderScoped
// order. ONLY for the owner-grouped scoped tables.
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

// orderScoped applies the listing order of the four owner-grouped scoped
// entities — decisions §5.29. id is the final tiebreak.
func orderScoped(q *bun.SelectQuery, ownerID string, admin bool) *bun.SelectQuery {
	if admin {
		return q.OrderExpr("created_at DESC, id DESC")
	}
	return q.OrderExpr("CASE WHEN owner_id = ? THEN 1 ELSE 0 END, created_at DESC, id DESC", ownerID)
}

// scopedListOrder is the SKILLS listing order: shared rows first, then newest
// first (decisions §5.29); id keeps same-instant rows in one order.
const scopedListOrder = `CASE WHEN scope = 'global' THEN 0 ELSE 1 END, created_at DESC, id DESC`

// ErrSameScope marks a scope flip refused because the row already holds the
// target scope (decisions §5.29). Handlers map it to 409.
var ErrSameScope = errors.New("the row is already in that scope")

// SetScopeOf moves one scoped row between global and private (decisions
// §5.29); the owner never changes. expectOwner is carried into the WHERE, so
// a transfer landing since the caller's check refuses the flip (ErrOwnershipChanged).
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
// matched nothing: gone, already in that scope, or moved to another owner.
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

// scopeOwnerOf reads the scope/owner pair off any scoped model by reflection
// (the five share the columns but no interface).
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
// moved since the caller's authorization (decisions §5.29). Handlers map it to 409.
var ErrOwnershipChanged = errors.New("the configuration's owner or scope changed; reload and try again")

// DeleteOwnedBy removes a row only while it still belongs to expectOwner (an
// admin names the owner they saw). ErrOwnershipChanged when it moved.
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

// SetOwnerOf transfers one scoped row to another user (admin). Scope is
// untouched; a name collision in the target namespace is a UNIQUE violation.
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
// name — the own-over-global tiebreak (decisions §5.29).
func Shadows(scope, rowOwner, callerID string) bool {
	return callerID != "" && scope == ScopePrivate && rowOwner == callerID
}

// RefVisible reports whether a holder row may REFERENCE the given row —
// decisions §5.29.
func RefVisible(refScope, refOwner, holderScope, holderOwner string) bool {
	if holderScope == ScopeGlobal {
		return refScope == ScopeGlobal
	}
	return refScope == ScopeGlobal || refOwner == holderOwner
}
