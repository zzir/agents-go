package compaction

import (
	"github.com/zzir/agents-go/agents/session"
)

// Index is a session's entries organized into groups, plus the accounting a
// strategy needs to decide what to drop.
//
// It is rebuilt incrementally: Update finds where it left off and groups only
// what arrived since. A long conversation regroups its whole history on every
// turn otherwise, which is wasted work that grows with the thing it is trying
// to shrink.
type Index struct {
	Groups    []*Group
	Estimator TokenEstimator

	turn int
}

// NewIndex groups entries. A nil estimator uses CharEstimator.
func NewIndex(entries []session.Entry, est TokenEstimator) *Index {
	if est == nil {
		est = CharEstimator{}
	}
	idx := &Index{Estimator: est}
	idx.Update(entries)
	return idx
}

// Update folds newly-arrived entries into the index.
//
// When the entries it already grouped are no longer a prefix of what it is
// given — a branch switch, a compaction, a fork, or an entirely different
// session — it rebuilds from scratch rather than trying to reconcile.
// Reconciling a rewritten history is where incremental indexes go wrong, and
// both rebuilding and the prefix check are cheap next to a model call.
func (idx *Index) Update(entries []session.Entry) {
	start, ok := idx.prefixMatches(entries)
	if !ok {
		// Not a continuation of what we hold: a different session, a branch
		// switch, a compaction, a fork. Rebuild rather than reconcile.
		idx.Groups = nil
		idx.turn = 0
		start = 0
	}
	if start >= len(entries) {
		return
	}
	// New entries mean a model call happened since the last pass, and its
	// usage measured the context as it then stood — without the groups already
	// excluded. Mark those exclusions settled so ContextTokens stops
	// subtracting what the newest measurement never counted.
	for _, g := range idx.Groups {
		if g.Excluded {
			g.settled = true
		}
	}
	idx.group(entries[start:])
}

// prefixMatches reports whether entries still begins with exactly the entries
// already grouped, and how many of them that is.
//
// The comparison is against the entries the groups already hold, field for
// field, rather than against a digest of some of their fields. An id alone
// would not do: entry ids are unique within a SESSION, not globally —
// FileSession numbers them e1, e2 with no session prefix at all — so a
// Compactor reused across sessions would see a matching id, treat the second
// conversation as a continuation of the first, and return one session's history
// to the other. Nor would a subset of the fields: ContextTokens reads Usage,
// so two sessions with identical text and different token counts must not
// share an index, or one gets the other's accounting and is compacted against
// the wrong budget.
func (idx *Index) prefixMatches(entries []session.Entry) (n int, ok bool) {
	for _, g := range idx.Groups {
		for _, have := range g.Entries {
			if n >= len(entries) || !have.Equal(entries[n]) {
				return 0, false
			}
			n++
		}
	}
	return n, true
}

// group folds entries into groups, appending to whatever the index already has.
func (idx *Index) group(entries []session.Entry) {
	i := 0
	for i < len(entries) {
		e := entries[i]
		kind, isCall, _, isReasoning := classify(e)

		// Reasoning looks ahead: a reasoning block followed by a tool call led
		// to that call, and separating them makes the replayed history
		// incoherent. This and the call/output pairing below are the two cases
		// the old hand-written split point existed to handle.
		if isReasoning {
			if j := nextConversational(entries, i+1); j >= 0 {
				if _, nextIsCall, _, _ := classify(entries[j]); nextIsCall {
					consumed := idx.appendToolCallGroup(entries, i)
					i += consumed
					continue
				}
			}
		}

		if isCall {
			consumed := idx.appendToolCallGroup(entries, i)
			i += consumed
			continue
		}

		if kind == GroupUser {
			idx.turn++
		}
		idx.add(kind, entries[i:i+1])
		i++
	}
}

// appendToolCallGroup consumes a tool call and everything that belongs with it
// — its outputs, sibling calls issued in the same turn, and any leading
// reasoning — adds them as one group, and reports how many entries it took.
func (idx *Index) appendToolCallGroup(entries []session.Entry, start int) int {
	end := idx.toolCallGroupEnd(entries, start)
	idx.add(GroupToolCall, entries[start:start+end])
	return end
}

// toolCallGroupEnd finds where the group starting at start ends.
func (idx *Index) toolCallGroupEnd(entries []session.Entry, start int) int {
	i := start
	open := map[string]bool{}
	sawCall := false

	for i < len(entries) {
		kind, isCall, isOutput, isReasoning := classify(entries[i])
		switch {
		case isReasoning && !sawCall:
			// Leading reasoning joins the group.
		case isCall:
			sawCall = true
			if id := probe(entries[i]).CallID; id != "" {
				open[id] = true
			}
		case isOutput:
			if id := probe(entries[i]).CallID; id != "" {
				delete(open, id)
			}
		case kind == GroupOther:
			// A non-conversation entry between a call and its output does not
			// break the pairing; keep going.
		default:
			// Anything else ends the group — but only once every call it
			// opened has its output, or the group would straddle the pairing
			// it exists to protect.
			if sawCall && len(open) == 0 {
				return i - start
			}
		}
		i++
	}
	return i - start
}

// nextConversational returns the index of the next entry that is part of the
// conversation, skipping annotations and other non-context records.
func nextConversational(entries []session.Entry, from int) int {
	for i := from; i < len(entries); i++ {
		if kind, _, _, _ := classify(entries[i]); kind != GroupOther {
			return i
		}
	}
	return -1
}

func (idx *Index) add(kind GroupKind, entries []session.Entry) {
	g := &Group{Kind: kind, Entries: append([]session.Entry(nil), entries...)}
	if kind != GroupSystem && kind != GroupOther {
		turn := idx.turn
		g.TurnIndex = &turn
	}
	for _, e := range entries {
		g.Tokens += idx.Estimator.Estimate(e)
	}
	idx.Groups = append(idx.Groups, g)
}

// IncludedGroups returns the groups still in the context.
func (idx *Index) IncludedGroups() []*Group {
	out := make([]*Group, 0, len(idx.Groups))
	for _, g := range idx.Groups {
		if !g.Excluded {
			out = append(out, g)
		}
	}
	return out
}

// IncludedEntries projects the index back to the entries that make up the
// context, with excluded groups dropped and replaced groups substituted.
func (idx *Index) IncludedEntries() []session.Entry {
	var out []session.Entry
	for _, g := range idx.Groups {
		if g.Excluded {
			// An excluded group may still contribute a stand-in — a folded
			// summary of the tool results it held.
			out = append(out, g.Replacement...)
			continue
		}
		out = append(out, g.Entries...)
	}
	return out
}

// Counts describes an index, for triggers and for reporting.
type Counts struct {
	Groups, IncludedGroups   int
	Entries, IncludedEntries int
	Turns                    int
	Tokens                   int
}

// Counts summarizes the index.
func (idx *Index) Counts() Counts {
	c := Counts{Tokens: idx.ContextTokens()}
	seenTurns := map[int]bool{}
	for _, g := range idx.Groups {
		c.Groups++
		c.Entries += len(g.Entries)
		if g.Excluded {
			continue
		}
		c.IncludedGroups++
		c.IncludedEntries += len(g.Entries)
		if g.TurnIndex != nil {
			seenTurns[*g.TurnIndex] = true
		}
	}
	c.Turns = len(seenTurns)
	return c
}

// ContextTokens estimates the included context's size.
//
// It is a hybrid, and that is the point. The provider's own usage number is
// authoritative for everything up to the last model call it covers; estimating
// that part again would replace a measurement with a guess. Only what came
// after has to be estimated, and that is a handful of entries rather than a
// whole conversation.
//
// The measurement predates THIS pass's exclusions: its InputTokens counted
// every group in the context when the call was made, including any a strategy
// has excluded since. Those unsettled exclusions are subtracted at their
// estimated size (their replacement, which now stands in, added back) —
// imprecise, but the point is movement: a number exclusion cannot lower keeps
// every threshold trigger firing all the way to the preserve floor, and
// a tokens-below Target can never be reached. Exclusions from earlier passes are
// settled by Update once a newer call has priced them in, and are not
// subtracted twice.
//
// With no usable usage anywhere — a fresh session, or one whose calls all
// failed — it falls back to estimating everything.
func (idx *Index) ContextTokens() int {
	// Locate the newest included entry carrying usage, and where it sits.
	usageGroup, usageEntry := -1, -1
	for gi := len(idx.Groups) - 1; gi >= 0 && usageGroup < 0; gi-- {
		g := idx.Groups[gi]
		if g.Excluded {
			continue
		}
		for ei := len(g.Entries) - 1; ei >= 0; ei-- {
			u := g.Entries[ei].Usage
			if u != nil && u.TotalTokens != 0 {
				usageGroup, usageEntry = gi, ei
				break
			}
		}
	}
	if usageGroup < 0 {
		total := 0
		for _, e := range idx.IncludedEntries() {
			total += idx.Estimator.Estimate(e)
		}
		return total
	}

	u := idx.Groups[usageGroup].Entries[usageEntry].Usage
	total := int(u.InputTokens + u.OutputTokens)
	for gi := range usageGroup {
		g := idx.Groups[gi]
		if g.Excluded && !g.settled {
			total -= g.Tokens
			for _, re := range g.Replacement {
				total += idx.Estimator.Estimate(re)
			}
		}
	}
	if total < 0 {
		total = 0
	}
	// Everything after the measured entry is estimated on top of it.
	for _, e := range idx.Groups[usageGroup].Entries[usageEntry+1:] {
		total += idx.Estimator.Estimate(e)
	}
	for _, g := range idx.Groups[usageGroup+1:] {
		if g.Excluded {
			for _, re := range g.Replacement {
				total += idx.Estimator.Estimate(re)
			}
			continue
		}
		for _, e := range g.Entries {
			total += idx.Estimator.Estimate(e)
		}
	}
	return total
}
