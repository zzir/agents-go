package compaction

import (
	"bytes"

	"github.com/zzir/agents-go/agents/session"
)

// TokenEstimator sizes an entry when no provider number is available.
type TokenEstimator interface {
	Estimate(session.Entry) int
}

// Character-count constants, measured in the pi project; estimates, not a bill.
const (
	// charsPerToken is the usual English text ratio.
	charsPerToken = 4
	// imageChars is what one image costs, charged as characters.
	imageChars = 4800
)

// CharEstimator sizes entries by their content length; deliberately crude, as a
// real tokenizer over the whole history every turn costs more than compaction saves.
type CharEstimator struct{}

// Estimate implements TokenEstimator.
func (CharEstimator) Estimate(e session.Entry) int {
	switch e.Kind {
	case session.EntryKindItem:
	case session.EntryKindCompaction:
		// A checkpoint contributes its payload; the entries it kept are
		// estimated as themselves.
		return len(e.Payload) / charsPerToken
	default:
		// Not sent to the model, so it costs nothing in context.
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
		// An unknown shape still costs what it weighs, or it would grow unbounded.
		chars = len(e.Item)
	}
	return chars / charsPerToken
}

// contentChars counts a content blob in bytes, not runes (CJK is ~3 bytes and
// ~1 token per character), plus the fixed cost per image.
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
