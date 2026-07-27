package agents

import "slices"

// PopMode says which entry a pop is looking for.
type PopMode int

const (
	// PopLast takes the session's most recent entry, whatever kind it is.
	PopLast PopMode = iota
	// PopLastItem takes the most recent conversation item, skipping past what
	// is not one — an error banner, a leaf move. It is "undo my last message".
	PopLastItem
)

// Removal is what a store must do to take one entry out: the entry itself, the
// ids to delete, and the parent links that have to move so the tree stays
// whole.
//
// It is one value rather than "which one" and "and then fix this" because they
// are one decision. Choosing an entry in the middle of a branch and deleting
// only that row leaves its children hanging off an id that is gone, and a walk
// that meets a missing parent stops there — so the session reads short, losing
// everything BEFORE the entry that was removed rather than just it.
type Removal struct {
	// Entry is what was removed, to hand back to the caller.
	Entry SessionEntry
	// Delete is every entry id to remove. More than one when a dependent
	// cannot be re-pointed at anything.
	Delete []string
	// Relink re-points entries at a new parent, by entry id. A store applies
	// these in the same step as the deletes.
	Relink map[string]string
}

// PlanPop decides what a pop removes, and what else has to change for the
// session to stay readable.
//
// entries are the ones the store considers live, in append order. It reports
// false when there is nothing to take.
//
// The two modes are separate because they answer separate questions, and one
// call answering both is how the same operation came to mean different things
// in different stores. PopLast is "undo the last thing that happened";
// PopLastItem is "undo the last thing that was said".
func PlanPop(entries []SessionEntry, mode PopMode) (Removal, bool) {
	target, ok := popTarget(entries, mode)
	if !ok {
		return Removal{}, false
	}
	return planRemoval(entries, target), true
}

// popTarget picks the entry a pop takes.
func popTarget(entries []SessionEntry, mode PopMode) (SessionEntry, bool) {
	if len(entries) == 0 {
		return SessionEntry{}, false
	}
	if mode == PopLast {
		// The newest entry, whatever it is. Nothing can be pointing at it —
		// a parent is always older — so this one never needs a repair.
		newest := entries[0]
		for _, e := range entries[1:] {
			if e.Seq > newest.Seq {
				newest = e
			}
		}
		return newest, true
	}
	// The newest item ON THE ACTIVE BRANCH. Append order would reach an item on
	// an abandoned attempt, which is not what anyone means by "my last
	// message": that attempt is already off the path.
	path := PathToLeaf(entries, LeafOf(entries))
	if len(path) == 0 {
		path = entries
	}
	// An entry a checkpoint folded away is skipped like a banner: it is not
	// part of the conversation as the model sees it, so it is not "my last
	// message" either. The items a pass KEPT remain reachable — they are on
	// the path and in the model's view, and a person undoing their last
	// message means one of those.
	folded := FoldedEntryIDs(path)
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Kind == EntryKindItem && !folded[path[i].ID] {
			return path[i], true
		}
	}
	return SessionEntry{}, false
}

// planRemoval works out what else must change when target goes.
func planRemoval(entries []SessionEntry, target SessionEntry) Removal {
	r := Removal{Entry: target, Delete: []string{target.ID}}
	for _, e := range entries {
		if e.ID == target.ID {
			continue
		}
		switch {
		case e.Kind == EntryKindLeaf:
			// A branch pointer at what is going: move it to where the branch
			// was before, which is what the tip becomes anyway.
			if p, err := e.LeafPayload(); err == nil && p.TargetID == target.ID {
				if r.Relink == nil {
					r.Relink = map[string]string{}
				}
				r.Relink[e.ID] = target.ParentID
			}
		case e.ParentID == target.ID:
			// A child hangs off an id that will not be there. Re-point it at
			// its grandparent so the walk closes the gap instead of stopping.
			if r.Relink == nil {
				r.Relink = map[string]string{}
			}
			r.Relink[e.ID] = target.ParentID
		}
	}
	return r
}

// ApplyRemoval returns entries with a Removal carried out, for a store that
// holds them in memory or rewrites them whole.
//
// A store that deletes and updates rows applies Delete and Relink itself, in
// one step — the point is that it applies BOTH.
func ApplyRemoval(entries []SessionEntry, r Removal) []SessionEntry {
	out := make([]SessionEntry, 0, len(entries))
	for _, e := range entries {
		if slices.Contains(r.Delete, e.ID) {
			continue
		}
		if parent, ok := r.Relink[e.ID]; ok {
			if e.Kind == EntryKindLeaf {
				if updated, err := e.WithLeafTarget(parent); err == nil {
					e = updated
				}
			} else {
				e.ParentID = parent
			}
		}
		out = append(out, e)
	}
	return out
}
