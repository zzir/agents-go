package compaction

// Trigger decides whether a strategy should act on an index; a strategy takes
// one to start and one to stop.
type Trigger func(*Index) bool

// Always fires unconditionally.
func Always() Trigger { return func(*Index) bool { return true } }

// Never fires.
func Never() Trigger { return func(*Index) bool { return false } }

// TokensExceed fires when the estimated context is larger than n.
func TokensExceed(n int) Trigger {
	return func(idx *Index) bool { return idx.ContextTokens() > n }
}

// GroupsExceed fires when more than n groups are still included.
func GroupsExceed(n int) Trigger {
	return func(idx *Index) bool { return idx.Counts().IncludedGroups > n }
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

// fires reports a trigger's verdict, treating nil as "no".
func fires(t Trigger, idx *Index) bool { return t != nil && t(idx) }

// reachedTarget reports whether a pass should stop: target, or with none, the
// trigger no longer firing.
func reachedTarget(target, trigger Trigger, idx *Index) bool {
	if target != nil {
		return target(idx)
	}
	return !fires(trigger, idx)
}
