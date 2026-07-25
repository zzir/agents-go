package agents

import (
	"context"
	"fmt"
)

// ForkSession extracts a session's active branch into another session,
// producing an independent conversation that shares its past.
//
// It is not "copy everything": abandoned branches stay behind, because the
// point of a fork is to continue from where the conversation actually is. Entry
// ids are preserved, so an update entry that names one still finds its target.
//
// dst is cleared first, so it holds exactly the extracted branch.
func ForkSession(ctx context.Context, src, dst *Session) error {
	path, err := src.PathEntries(ctx)
	if err != nil {
		return fmt.Errorf("fork: reading source session: %w", err)
	}
	return writeFork(ctx, dst, path)
}

// ForkSessionAt extracts the branch up to and including entryID, producing a
// point-in-time fork — "start over from here".
//
// entryID must be on the source's active branch. Choose it on a paired-item
// boundary: cutting between a function_call and its output leaves a dangling
// call, which the API rejects on the next run.
func ForkSessionAt(ctx context.Context, src, dst *Session, entryID string) error {
	all, err := src.Entries(ctx, Cursor{})
	if err != nil {
		return fmt.Errorf("fork: reading source session: %w", err)
	}
	path := PathToLeaf(all, entryID)
	if len(path) == 0 {
		return newUserError("fork: entry %q is not on the source session's history", entryID)
	}
	return writeFork(ctx, dst, path)
}

func writeFork(ctx context.Context, dst *Session, path []SessionEntry) error {
	if err := dst.Clear(ctx); err != nil {
		return fmt.Errorf("fork: clearing destination session: %w", err)
	}
	if len(path) == 0 {
		return nil
	}
	// Seq is the destination store's to assign; ids and parent links are the
	// conversation's and travel with it.
	forked := make([]SessionEntry, len(path))
	for i, e := range path {
		e.Seq = 0
		forked[i] = e
	}
	if err := dst.Append(ctx, forked...); err != nil {
		return fmt.Errorf("fork: writing to destination session: %w", err)
	}
	return nil
}
