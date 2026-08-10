package session

import (
	"context"
	"fmt"
)

// Fork extracts a session's active branch into another session, producing an
// independent conversation that shares its past.
//
// It is not "copy everything": abandoned branches stay behind. Entry ids are
// preserved, so an update that names one still finds its target. dst is cleared
// first, so it holds exactly the extracted branch.
func Fork(ctx context.Context, src, dst *Session) error {
	path, err := src.PathEntries(ctx)
	if err != nil {
		return fmt.Errorf("fork: reading source session: %w", err)
	}
	return writeFork(ctx, dst, path)
}

// A point-in-time fork — "start over from here" — is the composition of the
// exported pieces: PathToLeaf(entries, entryID) to cut the branch, then
// ReplaceEntries on the destination.
func writeFork(ctx context.Context, dst *Session, path []Entry) error {
	// Ids and parent links travel with the conversation; the destination
	// allocates the sequence numbers. One replace instead of clear-then-append
	// so an AtomicReplacer never shows a cleared-but-unfilled dst on a mid-write
	// failure.
	if err := ReplaceEntries(ctx, dst.storage, path...); err != nil {
		return fmt.Errorf("fork: writing destination session: %w", err)
	}
	return nil
}
