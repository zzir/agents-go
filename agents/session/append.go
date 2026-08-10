package session

import (
	"cmp"
	"fmt"
	"strconv"
	"time"
)

// AppendPoint is where a session stands when something is appended to it: what
// the next entry links to, and the highest sequence number it has ever handed
// out — two facts a backend can answer with a one-row query instead of reading
// the whole session.
type AppendPoint struct {
	// Leaf is the id of the entry the next one extends. Empty starts a root.
	Leaf string

	// LastSeq is the highest sequence number this session has issued — not the
	// highest it currently holds. They differ after a removal, and using the
	// second hands a number out twice. A backend that cannot tell them apart may
	// pass the highest it holds; SeqFor stays safe because the clock has moved on.
	LastSeq int64
}

// nowNanos is the clock PrepareAppend reads. A test replaces it to make the
// sequence numbers it produces predictable.
var nowNanos = func() int64 { return time.Now().UnixNano() }

// SeqFor returns the sequence number to give the first entry appended to a
// session whose append point is at.
//
// It reads the clock, not "one more than the last". A seq is a cursor position,
// which must never be handed out twice, must never move for an entry that
// stays, and must survive a clear or replace — none of which counting a result
// set gives (spec §2.5e2). The LastSeq guard covers a clock that does not move
// forward: a stepped clock, or two appends inside one nanosecond.
func SeqFor(at AppendPoint) int64 {
	return max(nowNanos(), at.LastSeq+1)
}

// seqOfEntryID reads the sequence claim out of a minted-form id ("e<seq>"),
// reporting false for ids of any other shape.
func seqOfEntryID(id string) (int64, bool) {
	if len(id) < 2 || id[0] != 'e' {
		return 0, false
	}
	n, err := strconv.ParseInt(id[1:], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// EntryIDFor returns the id of the entry at sequence number seq. It is derived
// from the sequence number, so both stay unique together. The form is opaque —
// nothing outside this file constructs or parses one.
func EntryIDFor(seq int64) string { return fmt.Sprintf("e%d", seq) }

// PrepareAppend fills in the fields a store owns — id, sequence number,
// creation time — and links each entry to the branch it extends.
//
// Backends call it so every store links and numbers identically. An entry that
// already carries an id keeps it (a fork re-adding a known entry keeps the
// identity an update points at), but every entry gets a fresh sequence number,
// strictly greater than anything issued before, so no cursor can skip a rewrite.
func PrepareAppend(entries []Entry, at AppendPoint) []Entry {
	// An imported id of the minted form e<seq> is a claim on that position:
	// joining it to the floor stops a destination whose clock trails the source
	// from later minting an id a preserved entry already holds (spec §2.5e2).
	for _, e := range entries {
		if n, ok := seqOfEntryID(e.ID); ok && n > at.LastSeq {
			at.LastSeq = n
		}
	}
	seq := SeqFor(at)
	out := make([]Entry, 0, len(entries))
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

// AppendPointOf reads a session's append point off the entries it holds — for
// backends that have them in hand anyway. LastSeq is a MAX, not a count; a
// backend that would have to read the whole session should query instead.
func AppendPointOf(entries []Entry) AppendPoint {
	at := AppendPoint{Leaf: LeafOf(entries)}
	for i := range entries {
		at.LastSeq = max(at.LastSeq, entries[i].Seq)
	}
	return at
}
