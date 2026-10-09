package session

import (
	"cmp"
	"fmt"
	"strconv"
	"time"
)

// AppendPoint is where a session stands when something is appended: the entry
// the next one links to, and the highest sequence number ever handed out.
type AppendPoint struct {
	// Leaf is the id of the entry the next one extends. Empty starts a root.
	Leaf string

	// LastSeq is the highest sequence number ever issued, not the highest still
	// held (they differ after a removal); SeqFor's clock floor tolerates the latter.
	LastSeq int64
}

// nowNanos is the clock PrepareAppend reads. A test replaces it to make the
// sequence numbers it produces predictable.
var nowNanos = func() int64 { return time.Now().UnixNano() }

// SeqFor returns the sequence number for the first entry appended at at: the
// clock, floored at LastSeq+1 for a clock that did not move — spec §2.5e2.
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

// EntryIDFor returns the id of the entry at sequence number seq. The form is
// opaque: nothing outside this file constructs or parses one — spec §2.5e2.
func EntryIDFor(seq int64) string { return fmt.Sprintf("e%d", seq) }

// PrepareAppend fills in the fields a store owns (id, sequence number, creation
// time) and links each entry to the branch it extends; every backend calls it.
// A non-empty id is kept; every entry gets a fresh sequence number — spec §2.5e2.
func PrepareAppend(entries []Entry, at AppendPoint) []Entry {
	// An imported minted-form id (e<seq>) raises the floor so a slower clock
	// cannot re-mint it.
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
			// A leaf move has no parent: it moves the tip to its target instead
			// of extending the branch.
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

// AppendPointOf reads the append point off entries a backend has in hand
// anyway; LastSeq is a MAX, not a count. Prefer a query to reading everything.
func AppendPointOf(entries []Entry) AppendPoint {
	at := AppendPoint{Leaf: LeafOf(entries)}
	for i := range entries {
		at.LastSeq = max(at.LastSeq, entries[i].Seq)
	}
	return at
}
