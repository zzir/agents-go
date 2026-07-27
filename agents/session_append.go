package agents

import (
	"cmp"
	"fmt"
	"time"
)

// AppendPoint is where a session stands when something is appended to it: what
// the next entry links to, and the highest sequence number it has ever handed
// out.
//
// It is two facts rather than the entries themselves because a backend that can
// answer them with a one-row query should not have to read the session to
// append to it.
type AppendPoint struct {
	// Leaf is the id of the entry the next one extends. Empty starts a root.
	Leaf string

	// LastSeq is the highest sequence number this session has issued — not the
	// highest it currently holds. They differ after a removal, and using the
	// second is how a sequence number gets handed out twice.
	//
	// A backend that cannot tell them apart passes the highest it holds:
	// SeqFor is written so that is still safe, because the clock has moved on
	// since the removed entry was written.
	LastSeq int64
}

// nowNanos is the clock PrepareAppend reads. A test replaces it to make the
// sequence numbers it produces predictable.
var nowNanos = func() int64 { return time.Now().UnixNano() }

// SeqFor returns the sequence number to give the first entry appended to a
// session whose append point is at.
//
// # Why this is not "one more than the last one"
//
// Seq is a cursor position: `Cursor.AfterSeq` means "everything you have not
// shown me". That makes exactly three demands, and counting satisfies none of
// them on its own.
//
//   - **It is never handed out twice.** Counting stored entries frees a number
//     as soon as one is removed, and the caller resuming from the last number
//     it saw then skips the next append — silently, and forever, because its
//     cursor is already past it.
//   - **It never moves for an entry that stays.** Numbering by position in a
//     result set shifts every survivor whenever a read filters one out, which
//     compaction and cross-model adaptation both do.
//   - **Clearing or replacing a history does not restart it.** A cursor outlives
//     the entries it pointed at, so a replacement numbered from the beginning
//     lands entirely before an existing cursor and is skipped in full.
//
// The clock satisfies all three without any state to persist: time does not go
// backwards when an entry is deleted. The guard against LastSeq covers the case
// where it does anyway — a stepped clock, or two appends inside one nanosecond
// — by refusing to issue a number this session has already used.
func SeqFor(at AppendPoint) int64 {
	return max(nowNanos(), at.LastSeq+1)
}

// EntryIDFor returns the id of the entry at sequence number seq.
//
// The id is derived from the sequence number rather than minted separately
// because the two need the same property — unique within the session, never
// handed out twice — and deriving one from the other means there is one thing
// to get right instead of two. It is opaque: nothing outside this file
// constructs or parses one, and a caller that needs an entry's id reads it
// back.
func EntryIDFor(seq int64) string { return fmt.Sprintf("e%d", seq) }

// PrepareAppend fills in the fields a store owns — id, sequence number,
// creation time — and links each entry to the branch it extends.
//
// Backends call it so every store links and numbers identically. Neither can be
// done by the caller: the id of the entry before is assigned here, so only this
// code knows what a parent link should point at, and the sequence numbers have
// to come from one allocator or they collide across backends that share a
// session's history through a fork or an export.
//
// An entry that already carries an id keeps it — a fork or a replace re-adding
// a known entry keeps the identity an update entry points at — but every entry
// gets a fresh sequence number, which is strictly greater than anything issued
// before, so no cursor can skip what a rewrite produced.
func PrepareAppend(entries []SessionEntry, at AppendPoint) []SessionEntry {
	seq := SeqFor(at)
	out := make([]SessionEntry, 0, len(entries))
	parent := at.Leaf
	now := time.Now().UTC()
	for _, e := range entries {
		e.Seq = seq
		seq++
		if e.ID == "" {
			e.ID = EntryIDFor(e.Seq)
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		e.Kind = cmp.Or(e.Kind, EntryKindItem)
		if e.Kind == EntryKindLeaf {
			// A leaf move is a marker, not a node: it has no parent, and it
			// moves the tip to its target rather than extending the branch.
			if p, err := e.LeafPayload(); err == nil {
				parent = p.TargetID
			}
			out = append(out, e)
			continue
		}
		e.ParentID = cmp.Or(e.ParentID, parent)
		parent = e.ID
		out = append(out, e)
	}
	return out
}

// AppendPointOf reads a session's append point off the entries it holds.
//
// It is for backends that have them in hand anyway. One that would have to read
// the whole session to call this should answer the two questions with its own
// queries instead — LastSeq in particular is a MAX, not a count.
func AppendPointOf(entries []SessionEntry) AppendPoint {
	at := AppendPoint{Leaf: LeafOf(entries)}
	for i := range entries {
		at.LastSeq = max(at.LastSeq, entries[i].Seq)
	}
	return at
}
