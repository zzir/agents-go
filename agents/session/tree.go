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
// — the single branch that leaf belongs to, abandoned siblings left out. A
// compaction checkpoint does not end the walk (spec §2.5d).
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
			// A missing parent (folded away) ends the walk; a repeat is a cycle
			// nothing should produce — stop so a corrupt session reads short.
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

// ActiveBranchOf returns the entries a branch-scoped view reads: the walk to
// the active leaf, extended over the linkless prefix ahead of its root. When
// links exist and the walk is empty, empty is the answer — spec §2.5d.
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
		// A leaf move is a pointer, not a node: the walk excludes it, so branching
		// to one would leave the session with no active branch.
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
