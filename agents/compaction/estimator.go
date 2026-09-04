package compaction

import (
	"bytes"

	"github.com/zzir/agents-go/agents/session"
)

// TokenEstimator sizes an entry when no provider number is available.
type TokenEstimator interface {
	Estimate(session.Entry) int
}

// Character-count constants, from measurements in the pi project. They are
// estimates by construction: roughly right about what to drop, not a bill.
const (
	// charsPerToken is the usual English text ratio.
	charsPerToken = 4
	// imageChars is what one image costs, charged as characters; an image is far
	// larger than its JSON suggests.
	imageChars = 4800
)

// CharEstimator sizes entries by their content length. Deliberately crude: a
// real tokenizer over the whole history every turn costs more than compaction
// saves, and "is this too big" needs only the order of magnitude.
type CharEstimator struct{}

// Estimate implements TokenEstimator.
func (CharEstimator) Estimate(e session.Entry) int {
	switch e.Kind {
	case session.EntryKindItem:
	case session.EntryKindCompaction:
		// A checkpoint's payload (summary and fold stand-ins) is what it
		// contributes; the entries it kept are estimated as themselves.
		return len(e.Payload) / charsPerToken
	default:
		// Annotations, terminal records and the like are not sent to the model,
		// so they cost nothing in context.
		return 0
	}

	p := session.ProbeItem(e.Item)
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

// contentChars counts a content blob in BYTES, not runes: CJK runs ~3 bytes per
// character and roughly one token, so bytes/4 lands near the truth. Images are charged their fixed cost.
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
