package session

import (
	"context"
	"fmt"
)

// LeafOf folds a session's entries down to the id of its active branch tip. It
// is derived, not stored, so nothing can disagree with the log. A leaf entry
// moves the tip to its target; anything else becomes the tip.
func LeafOf(entries []Entry) string {
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
// A compaction checkpoint does NOT end the walk: a folded entry is still on its
// branch. What the model sees is a separate question, answered by ProjectEntries
// applying the checkpoint's exclusions.
func PathToLeaf(entries []Entry, leafID string) []Entry {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.Kind == EntryKindLeaf {
			continue
		}
		byID[e.ID] = e
	}

	var reversed []Entry
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

	out := make([]Entry, len(reversed))
	for i, e := range reversed {
		out[len(reversed)-1-i] = e
	}
	return out
}

// ActiveBranchOf returns the entries a branch-scoped view should read: the walk
// to the active leaf, extended over the LINKLESS PREFIX ahead of it.
//
// Entries written before branching existed carry no parent links, so each is
// its own root and the walk reaches only the last one. Reading that linkless run
// back in is what keeps the upgrade path from losing history. A linked entry
// ahead of the walk's root belongs to another branch and stays out; and when
// links exist but the walk comes back empty (a leaf move whose target is gone),
// empty is the honest answer — falling back to append order would leak abandoned
// attempts into the model's view.
func ActiveBranchOf(entries []Entry) []Entry {
	path := PathToLeaf(entries, LeafOf(entries))
	if len(path) == 0 || path[0].ParentID != "" {
		return path
	}
	// Where the walk's root sits in append order, and how far back the
	// linkless run before it reaches.
	root := len(entries)
	for i := range entries {
		if entries[i].ID == path[0].ID && entries[i].Kind != EntryKindLeaf {
			root = i
			break
		}
	}
	start := root
	for i := root - 1; i >= 0; i-- {
		if entries[i].Kind == EntryKindLeaf || entries[i].ParentID != "" {
			break
		}
		start = i
	}
	if start == root {
		return path
	}
	out := make([]Entry, 0, (root-start)+len(path))
	out = append(out, entries[start:root]...)
	return append(out, path...)
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
// continues from there. Everything after the old tip stays recorded — "try that
// again differently" without deleting anything.
func (s *Session) Branch(ctx context.Context, entryID string) error {
	target, err := s.storage.Entry(ctx, entryID)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("session: branch: no entry %q in this session", entryID)
	}
	if target.Kind == EntryKindLeaf {
		// A leaf move is a pointer, not a place: the walk excludes it from the
		// tree, so a branch "to" one resolves the tip to an id that is not a
		// node and the session reads as having no active branch.
		return fmt.Errorf("session: branch: entry %q is a branch move, not an entry to branch to", entryID)
	}
	leaf, err := NewLeafEntry(entryID)
	if err != nil {
		return err
	}
	return s.storage.Append(ctx, leaf)
}

// PathEntries returns the entries on the active branch, oldest-first. A flat,
// linkless history is one branch and reads whole (see ActiveBranchOf).
func (s *Session) PathEntries(ctx context.Context) ([]Entry, error) {
	entries, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, err
	}
	return ActiveBranchOf(entries), nil
}
