package store

import "errors"

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
