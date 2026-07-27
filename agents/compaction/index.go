package compaction

import (
	"hash/fnv"

	"github.com/zzir/agents-go/agents"
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

	// indexed fingerprints every entry already grouped, in order. Update
	// resumes only when what it is handed still starts with exactly these.
	//
	// An id alone is not enough to decide that. Entry ids are unique within a
	// SESSION, not globally — FileSession numbers them e1, e2 with no session
	// prefix at all — so a Compactor reused across sessions would see a
	// matching id, treat the second conversation as a continuation of the
	// first, and return one session's history to the other.
	indexed []uint64
	turn    int
}

// fingerprint identifies an entry by what it holds, not just by its id.
func fingerprint(e agents.SessionEntry) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(e.ID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(e.Kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(e.Item)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(e.Payload)
	return h.Sum64()
}

// NewIndex groups entries. A nil estimator uses CharEstimator.
func NewIndex(entries []agents.SessionEntry, est TokenEstimator) *Index {
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
func (idx *Index) Update(entries []agents.SessionEntry) {
	start := len(idx.indexed)
	if start > len(entries) || !idx.prefixMatches(entries) {
		// Not a continuation of what we hold: a different session, a branch
		// switch, a compaction, a fork. Rebuild rather than reconcile.
		idx.Groups = nil
		idx.turn = 0
		idx.indexed = nil
		start = 0
	}
	if start >= len(entries) {
		return
	}
	for _, e := range entries[start:] {
		idx.indexed = append(idx.indexed, fingerprint(e))
	}
	idx.group(entries[start:])
}

// prefixMatches reports whether entries still begins with exactly what the
// index already grouped.
func (idx *Index) prefixMatches(entries []agents.SessionEntry) bool {
	for i, want := range idx.indexed {
		if fingerprint(entries[i]) != want {
			return false
		}
	}
	return true
}

// group folds entries into groups, appending to whatever the index already has.
func (idx *Index) group(entries []agents.SessionEntry) {
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
func (idx *Index) appendToolCallGroup(entries []agents.SessionEntry, start int) int {
	end := idx.toolCallGroupEnd(entries, start)
	idx.add(GroupToolCall, entries[start:start+end])
	return end
}

// toolCallGroupEnd finds where the group starting at start ends.
func (idx *Index) toolCallGroupEnd(entries []agents.SessionEntry, start int) int {
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
func nextConversational(entries []agents.SessionEntry, from int) int {
	for i := from; i < len(entries); i++ {
		if kind, _, _, _ := classify(entries[i]); kind != GroupOther {
			return i
		}
	}
	return -1
}

func (idx *Index) add(kind GroupKind, entries []agents.SessionEntry) {
	g := &Group{Kind: kind, Entries: append([]agents.SessionEntry(nil), entries...)}
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
func (idx *Index) IncludedEntries() []agents.SessionEntry {
	var out []agents.SessionEntry
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
// With no usable usage anywhere — a fresh session, or one whose calls all
// failed — it falls back to estimating everything.
func (idx *Index) ContextTokens() int {
	entries := idx.IncludedEntries()

	// Walk back to the most recent entry carrying usage. Its InputTokens
	// already accounts for the history the model was sent, so nothing before it
	// needs estimating.
	for i := len(entries) - 1; i >= 0; i-- {
		u := entries[i].Usage
		if u == nil || u.TotalTokens == 0 {
			continue
		}
		total := int(u.InputTokens + u.OutputTokens)
		for _, later := range entries[i+1:] {
			total += idx.Estimator.Estimate(later)
		}
		return total
	}

	total := 0
	for _, e := range entries {
		total += idx.Estimator.Estimate(e)
	}
	return total
}
