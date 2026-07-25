package compaction

// Trigger decides whether a strategy should act on an index.
//
// Strategies take two: one to start and one to stop. Separating them is what
// makes a pass stop at a comfortable size rather than at the threshold that
// alarmed it — compacting down to exactly the trigger point means the next turn
// triggers again, and the conversation spends the rest of its life compacting.
type Trigger func(*Index) bool

// Always fires unconditionally.
func Always() Trigger { return func(*Index) bool { return true } }

// Never fires.
func Never() Trigger { return func(*Index) bool { return false } }

// TokensExceed fires when the estimated context is larger than n.
func TokensExceed(n int) Trigger {
	return func(idx *Index) bool { return idx.ContextTokens() > n }
}

// TokensBelow fires when the estimated context is smaller than n. It is the
// usual Target: stop once the history is comfortably under budget.
func TokensBelow(n int) Trigger {
	return func(idx *Index) bool { return idx.ContextTokens() < n }
}

// TurnsExceed fires when more than n conversation turns are still included.
func TurnsExceed(n int) Trigger {
	return func(idx *Index) bool { return idx.Counts().Turns > n }
}

// GroupsExceed fires when more than n groups are still included.
func GroupsExceed(n int) Trigger {
	return func(idx *Index) bool { return idx.Counts().IncludedGroups > n }
}

// EntriesExceed fires when more than n entries are still included.
func EntriesExceed(n int) Trigger {
	return func(idx *Index) bool { return idx.Counts().IncludedEntries > n }
}

// HasToolCalls fires when any included group holds tool calls — the cheap
// compaction opportunity.
func HasToolCalls() Trigger {
	return func(idx *Index) bool {
		for _, g := range idx.IncludedGroups() {
			if g.Kind == GroupToolCall {
				return true
			}
		}
		return false
	}
}

// All fires when every trigger does.
func All(triggers ...Trigger) Trigger {
	return func(idx *Index) bool {
		for _, t := range triggers {
			if t == nil || !t(idx) {
				return false
			}
		}
		return true
	}
}

// Any fires when at least one trigger does.
func Any(triggers ...Trigger) Trigger {
	return func(idx *Index) bool {
		for _, t := range triggers {
			if t != nil && t(idx) {
				return true
			}
		}
		return false
	}
}

// Not inverts a trigger.
func Not(t Trigger) Trigger {
	return func(idx *Index) bool { return t != nil && !t(idx) }
}

// fires reports a trigger's verdict, treating nil as "no".
func fires(t Trigger, idx *Index) bool { return t != nil && t(idx) }

// reachedTarget reports whether a pass should stop. With no explicit target,
// stopping means the trigger no longer fires — which is the minimum that avoids
// an immediate re-trigger, though rarely the best choice.
func reachedTarget(target, trigger Trigger, idx *Index) bool {
	if target != nil {
		return target(idx)
	}
	return !fires(trigger, idx)
}
