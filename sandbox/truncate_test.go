package sandbox

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The tail is the half that matters: a failing build prints progress first and
// the error last. Head-only truncation would drop exactly the verdict.
func TestTruncateWithInfoKeepsHeadAndTail(t *testing.T) {
	body := strings.Repeat("x", 500)
	s := "BUILD START\n" + body + "\nFAILED: undefined symbol"

	got := truncateWithInfo(s, 100)
	if !strings.HasPrefix(got, "BUILD START") {
		t.Errorf("head was lost: %q", got[:min(40, len(got))])
	}
	if !strings.HasSuffix(got, "FAILED: undefined symbol") {
		t.Errorf("tail was lost: %q", got[max(0, len(got)-40):])
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("no elision marker: %q", got)
	}
}

func TestTruncateWithInfoPassesThroughShortInput(t *testing.T) {
	for _, s := range []string{"", "short", strings.Repeat("y", 100)} {
		if got := truncateWithInfo(s, 100); got != s {
			t.Errorf("truncateWithInfo(%d bytes, 100) modified input", len(s))
		}
	}
	if got := truncateWithInfo("anything", 0); got != "anything" {
		t.Errorf("limit 0 should disable truncation, got %q", got)
	}
}

// Both cut points must land on rune boundaries — the head must not end
// mid-sequence and the tail must not begin mid-sequence.
func TestTruncateWithInfoRespectsRuneBoundaries(t *testing.T) {
	// 3-byte runes so almost every byte offset is mid-sequence.
	s := strings.Repeat("世", 200)
	for limit := 4; limit < 120; limit++ {
		got := truncateWithInfo(s, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8", limit)
		}
	}
	// Mixed widths, tail starting inside a rune.
	mixed := strings.Repeat("a", 50) + strings.Repeat("→", 50)
	for limit := 4; limit < 100; limit++ {
		if got := truncateWithInfo(mixed, limit); !utf8.ValidString(got) {
			t.Fatalf("limit %d on mixed-width input produced invalid UTF-8", limit)
		}
	}
}

// The head and tail must not overlap, or the output would repeat content and
// report a negative elision.
func TestTruncateWithInfoNoOverlap(t *testing.T) {
	s := "0123456789ABCDEFGHIJ"
	for limit := 1; limit < len(s); limit++ {
		got := truncateWithInfo(s, limit)
		if strings.Contains(got, "elided") {
			head, tail, _ := strings.Cut(got, "\n…[")
			_, tail, _ = strings.Cut(tail, "]\n")
			if len(head)+len(tail) > len(s) {
				t.Errorf("limit %d: head+tail (%d) exceeds input (%d): %q", limit, len(head)+len(tail), len(s), got)
			}
		}
	}
}
