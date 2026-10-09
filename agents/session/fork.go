package session

import (
	"context"
	"fmt"
)

// Fork extracts src's active branch into dst, replacing dst's history; entry
// ids are kept so an update still finds its target — spec §2.5d.
func Fork(ctx context.Context, src, dst *Session) error {
	path, err := src.PathEntries(ctx)
	if err != nil {
		return fmt.Errorf("fork: reading source session: %w", err)
	}
	return writeFork(ctx, dst, path)
}

// writeFork writes path as dst's whole history. A point-in-time fork is
// PathToLeaf(entries, id) to cut the branch, then ReplaceEntries on dst.
func writeFork(ctx context.Context, dst *Session, path []Entry) error {
	// One replace, never clear-then-append — spec §2.5d.
	if err := ReplaceEntries(ctx, dst.storage, path...); err != nil {
		return fmt.Errorf("fork: writing destination session: %w", err)
	}
	return nil
}
