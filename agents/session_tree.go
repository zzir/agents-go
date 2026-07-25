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
// A compaction checkpoint ends the walk: it stands in for everything before it,
// so nothing older is part of the context any more.
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
		if e.Kind == EntryKindCompaction {
			break
		}
		id = e.ParentID
	}

	out := make([]SessionEntry, len(reversed))
	for i, e := range reversed {
		out[len(reversed)-1-i] = e
	}
	return out
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
	leaf, err := NewLeafEntry(entryID)
	if err != nil {
		return err
	}
	return s.storage.Append(ctx, leaf)
}

// PathEntries returns the entries on the active branch, oldest-first.
func (s *Session) PathEntries(ctx context.Context) ([]SessionEntry, error) {
	entries, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, err
	}
	leaf := LeafOf(entries)
	if leaf == "" {
		return nil, nil
	}
	return PathToLeaf(entries, leaf), nil
}
