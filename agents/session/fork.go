package session

import (
	"context"
	"fmt"
)

// Fork extracts a session's active branch into another session,
// producing an independent conversation that shares its past.
//
// It is not "copy everything": abandoned branches stay behind, because the
// point of a fork is to continue from where the conversation actually is. Entry
// ids are preserved, so an update entry that names one still finds its target.
//
// dst is cleared first, so it holds exactly the extracted branch.
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
	// Ids and parent links are the conversation's and travel with it; the
	// destination allocates the sequence numbers, since a cursor position
	// belongs to the session it pages. One replace instead of clear-then-append
	// so a storage that can swap atomically (AtomicReplacer) never shows a
	// cleared-but-unfilled dst when a failure lands mid-write.
	if err := ReplaceEntries(ctx, dst.storage, path...); err != nil {
		return fmt.Errorf("fork: writing destination session: %w", err)
	}
	return nil
}
