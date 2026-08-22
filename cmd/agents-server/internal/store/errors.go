package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
)

// ErrNotFound reports that the requested row does not exist. Store methods
// wrap it (errors.Is-compatible) so handlers can map it to a 404 instead of
// treating every store error as internal.
var ErrNotFound = errors.New("not found")

// rowsAffected is the subset of sql.Result the not-found checks need.
type rowsAffected interface {
	RowsAffected() (int64, error)
}

// requireRows returns ErrNotFound when res reports zero affected rows.
// Drivers that can't report affected rows (err != nil) are treated as success.
func requireRows(res rowsAffected) error {
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// UniqueViolation reports the offending column list (e.g. "name" or
// "type, name", table prefixes stripped) and true when err is a UNIQUE
// constraint failure, letting handlers map a duplicate to 409 without a
// racy pre-check. SQLite is matched best-effort by message, so it works
// across sqlite drivers; PostgreSQL by SQLSTATE.
func UniqueViolation(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) && pgErr.Field('C') == "23505" {
		// DETAIL reads `Key (col[, col...])=(...) already exists.`; fall back
		// to the constraint name when the server withholds it.
		detail := pgErr.Field('D')
		if _, rest, ok := strings.Cut(detail, "Key ("); ok {
			if cols, _, ok := strings.Cut(rest, ")="); ok {
				return cols, true
			}
		}
		return pgErr.Field('n'), true
	}
	const marker = "UNIQUE constraint failed: "
	i := strings.Index(err.Error(), marker)
	if i < 0 {
		return "", false
	}
	rest := err.Error()[i+len(marker):]
	if end := strings.IndexAny(rest, "\n\r"); end >= 0 {
		rest = rest[:end]
	}
	// rest is "table.col[, table.col...]", sometimes with a trailing driver
	// suffix like " (2067)". Strip table prefixes and keep only the column
	// identifiers.
	cols := make([]string, 0, 2)
	for part := range strings.SplitSeq(rest, ",") {
		part = strings.TrimSpace(part)
		if _, after, ok := strings.CutLast(part, "."); ok {
			part = after
		}
		part = identifierPrefix(part)
		if part != "" {
			cols = append(cols, part)
		}
	}
	return strings.Join(cols, ", "), len(cols) > 0
}

// identifierPrefix returns the leading run of identifier characters, dropping
// any trailing driver noise (e.g. a " (2067)" error-code suffix).
func identifierPrefix(s string) string {
	for i, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return s[:i]
		}
	}
	return s
}

// updateColumn sets a single column on the row identified by id, enforcing
// that the row existed (ErrNotFound otherwise) so a silent no-op update can't
// masquerade as success. label names the entity for error messages.
func updateColumn(ctx context.Context, db *bun.DB, model any, label, id, column string, value any) error {
	res, err := db.NewUpdate().Model(model).Set(column+" = ?", value).Where("id = ?", id).Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", label, id, err)
	}
	return nil
}

// IsMalformedID reports whether err is PostgreSQL refusing a value that is not
// a UUID for a uuid column (SQLSTATE 22P02) — a path parameter like
// /sessions/abc. SQLite stores anything, so only PostgreSQL raises it; handlers
// answer 400 rather than 500.
func IsMalformedID(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "22P02"
}
