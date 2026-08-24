package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// The visibility vocabulary of scoped configuration (spec §5.29): providers,
// agent configs, MCP servers, skills and workflows carry a Scope and, when
// private, an OwnerID.
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

// NormalizeScope pins the scope/owner invariant at write time: private rows
// carry their owner, global rows carry none. An empty scope reads as private
// — the default for every creator; global is an explicit act.
func NormalizeScope(scope, ownerID string) (string, string) {
	if scope == ScopeGlobal {
		return ScopeGlobal, ""
	}
	return ScopePrivate, ownerID
}

// ListVisibleOf returns the scoped-entity rows ownerID may see, in the
// listing order spec §5.29 promises (global first, both groups by creation
// time). ONLY for the five scoped tables; a table without the scope/owner
// columns fails the query loudly.
func ListVisibleOf[T any](ctx context.Context, s *CrudStore[T], ownerID string, admin bool) ([]T, error) {
	var out []T
	q := visibleTo(s.db.NewSelect().Model(&out), ownerID, admin).
		OrderExpr("CASE WHEN scope = ? THEN 0 ELSE 1 END", ScopeGlobal).
		OrderExpr("created_at ASC").
		OrderExpr("id ASC") // same-instant rows keep one order across reloads
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

// SetScopeOf moves one scoped row between global and private (spec §5.29).
// The unique name indexes decide collisions: promoting a private "build"
// beside a global "build" fails with a UNIQUE violation the handler maps to
// 409.
func SetScopeOf[T any](ctx context.Context, s *CrudStore[T], id, scope, ownerID string) error {
	res, err := s.db.NewUpdate().Model((*T)(nil)).
		Set("scope = ?", scope).
		Set("owner_id = ?", uuidOrNull(ownerID)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("setting %s %s scope: %w", s.label, id, err)
	}
	return nil
}

// RefVisible reports whether a holder row may REFERENCE the given row: a
// global holder only global rows, a private holder global rows plus its
// owner's own — spec §5.29.
func RefVisible(refScope, refOwner, holderScope, holderOwner string) bool {
	if holderScope == ScopeGlobal {
		return refScope == ScopeGlobal
	}
	return refScope == ScopeGlobal || refOwner == holderOwner
}
