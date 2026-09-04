package session

import (
	"context"
	"fmt"
)

// Fork extracts a session's active branch into another session: abandoned
// branches stay behind, entry ids are preserved so an update still finds its
// target, and dst is cleared first (spec §2.5d).
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
	// One replace, not clear-then-append, so an AtomicReplacer never shows a
	// cleared-but-unfilled dst on a mid-write failure (spec §2.5d).
	if err := ReplaceEntries(ctx, dst.storage, path...); err != nil {
		return fmt.Errorf("fork: writing destination session: %w", err)
	}
	return nil
}
