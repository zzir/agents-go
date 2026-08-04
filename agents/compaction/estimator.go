package compaction

import (
	"bytes"

	"github.com/zzir/agents-go/agents"
)

// TokenEstimator sizes an entry when no provider number is available.
type TokenEstimator interface {
	Estimate(agents.SessionEntry) int
}

// Character-count constants, taken from measurements in the pi project rather
// than invented here. They are estimates by construction: the point is to be
// roughly right about what to drop, not to predict a bill.
const (
	// charsPerToken is the usual English text ratio.
	charsPerToken = 4
	// imageChars is what one image costs, charged as characters so it flows
	// through the same arithmetic. An image is far larger than its JSON
	// suggests, and treating it as its byte length would badly under-count.
	imageChars = 4800
)

// CharEstimator sizes entries by their content length.
//
// It is deliberately crude. Running a real tokenizer over the whole history on
// every turn costs more than the compaction saves, and the decision it feeds —
// "is this conversation too big" — does not need precision, only the right
// order of magnitude. Inject a real tokenizer when it does.
type CharEstimator struct{}

// Estimate implements TokenEstimator.
func (CharEstimator) Estimate(e agents.SessionEntry) int {
	switch e.Kind {
	case agents.EntryKindItem:
	case agents.EntryKindCompaction:
		// A checkpoint's payload holds its summary and fold stand-ins, which
		// is what it contributes to the context. (The entries it kept are
		// estimated as themselves; they are in the session, not in here.)
		return len(e.Payload) / charsPerToken
	default:
		// Annotations, terminal records and the like are not sent to the model,
		// so they cost nothing in context.
		return 0
	}

	p := probe(e)
	chars := 0
	if p.Name != "" {
		chars += len(p.Name)
	}
	chars += len(p.Args)
	chars += contentChars(p.Content)
	chars += contentChars(p.Output)
	chars += contentChars(p.Summary)
	if chars == 0 {
		// A shape this estimator does not understand still costs what it
		// weighs; counting zero would let unknown items grow unbounded.
		chars = len(e.Item)
	}
	return chars / charsPerToken
}

// contentChars counts a content blob by its byte length — deliberately bytes,
// not runes: CJK text runs ~3 bytes per character and roughly one token, so
// bytes/4 lands near the truth where runes/4 would under-count it fourfold.
// Images are charged their fixed cost instead of the length of the URL that
// names them.
func contentChars(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	chars := len(raw)
	// Every image part replaces its JSON with the fixed image cost.
	for _, marker := range [][]byte{[]byte(`"input_image"`), []byte(`"image_url"`)} {
		chars += imageChars * bytes.Count(raw, marker)
	}
	return chars
}
