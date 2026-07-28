package agents

import "context"

// LeafOf folds a session's entries down to the id of its active branch tip.
//
// It is derived, not stored. A pointer kept beside the log would have to be
// updated on every append and could disagree with the log after a crash or a
// concurrent writer; folding cannot. The rule is one line: a leaf entry moves
// the tip to its target, anything else becomes the tip itself.
func LeafOf(entries []SessionEntry) string {
	leaf := ""
	for _, e := range entries {
		if e.Kind == EntryKindLeaf {
			if p, err := e.LeafPayload(); err == nil {
				leaf = p.TargetID
			}
			continue
		}
		leaf = e.ID
	}
	return leaf
}

// PathToLeaf returns the entries from the root to the given leaf, oldest-first
// — the single branch that leaf belongs to, with every abandoned sibling left
// out.
//
// A compaction checkpoint does NOT end the walk. The walk answers "which
// entries are on this branch", and an entry folded by compaction is still on
// the branch it was written to — what the MODEL sees is a separate question,
// answered by ProjectEntries applying the checkpoint's exclusions. Ending the
// walk here is what once made everything behind a checkpoint unreachable to a
// pop, while the model could still see it.
func PathToLeaf(entries []SessionEntry, leafID string) []SessionEntry {
	byID := make(map[string]SessionEntry, len(entries))
	for _, e := range entries {
		if e.Kind == EntryKindLeaf {
			continue
		}
		byID[e.ID] = e
	}

	var reversed []SessionEntry
	seen := make(map[string]bool, len(entries))
	for id := leafID; id != ""; {
		e, ok := byID[id]
		if !ok || seen[id] {
			// A missing parent ends the walk rather than failing: an ancestor
			// may have been folded away by compaction. A repeat means the
			// stored parent links form a cycle, which nothing should produce —
			// stopping keeps a corrupt session readable instead of hanging.
			break
		}
		seen[id] = true
		reversed = append(reversed, e)
		id = e.ParentID
	}

	out := make([]SessionEntry, len(reversed))
	for i, e := range reversed {
		out[len(reversed)-1-i] = e
	}
	return out
}

// activeBranchOf returns the entries a branch-scoped view should read:
// PathToLeaf's walk, with the flat history handled. Entries carrying no tree
// links at all — no parent ids, no leaf moves — are one straight line (a
// session written before branching existed, or a server-held store that has
// no tree), and walking them as a tree would read only the last entry: every
// parent link is empty, so the walk stops after one step. The whole list IS
// that session's branch.
//
// When links exist and the walk still comes back empty — a leaf move whose
// target is gone — the active branch genuinely points at nothing, and empty
// is the honest answer: falling back to append order here is how abandoned
// attempts once leaked into the model's view.
func activeBranchOf(entries []SessionEntry) []SessionEntry {
	for _, e := range entries {
		if e.ParentID != "" || e.Kind == EntryKindLeaf {
			return PathToLeaf(entries, LeafOf(entries))
		}
	}
	return entries
}

// Leaf returns the id of the session's active branch tip.
func (s *Session) Leaf(ctx context.Context) (string, error) {
	entries, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return "", err
	}
	return LeafOf(entries), nil
}

// Branch moves the session's active branch to entryID, so the next append
// continues from there. Everything after the old tip stays recorded.
//
// It is how "try that again differently" works without deleting anything: the
// abandoned attempt is still in the log, just no longer on the path.
func (s *Session) Branch(ctx context.Context, entryID string) error {
	target, err := s.storage.Entry(ctx, entryID)
	if err != nil {
		return err
	}
	if target == nil {
		return newUserError("branch: no entry %q in this session", entryID)
	}
	if target.Kind == EntryKindLeaf {
		// A leaf move is a pointer, not a place: the walk excludes it from the
		// tree, so a branch "to" one resolves the tip to an id that is not a
		// node and the session reads as having no active branch.
		return newUserError("branch: entry %q is a branch move, not an entry to branch to", entryID)
	}
	leaf, err := NewLeafEntry(entryID)
	if err != nil {
		return err
	}
	return s.storage.Append(ctx, leaf)
}

// PathEntries returns the entries on the active branch, oldest-first. A flat,
// linkless history is one branch and reads whole (see activeBranchOf).
func (s *Session) PathEntries(ctx context.Context) ([]SessionEntry, error) {
	entries, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, err
	}
	return activeBranchOf(entries), nil
}
