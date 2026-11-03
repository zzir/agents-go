package sandbox

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// headTail is an io.Writer that keeps a stream's first and last bytes past its
// capacity — the head 60%, the tail 40%, truncateWithInfo's split — and counts
// every byte written (spec §2.7v).
type headTail struct {
	headMax, tailMax int
	head, tail       []byte
	total            int64
}

func newHeadTail(capacity int) *headTail {
	headMax := capacity * 3 / 5
	return &headTail{headMax: headMax, tailMax: capacity - headMax}
}

func (c *headTail) Write(p []byte) (int, error) {
	n := len(p)
	c.total += int64(n)
	if room := c.headMax - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	switch {
	case len(p) == 0:
	case len(p) >= c.tailMax:
		c.tail = append(c.tail[:0], p[len(p)-c.tailMax:]...)
	default:
		c.tail = append(c.tail, p...)
		// Trimmed once it doubles, not on every write.
		if len(c.tail) > 2*c.tailMax {
			c.tail = c.tail[:copy(c.tail, c.tail[len(c.tail)-c.tailMax:])]
		}
	}
	return n, nil
}

// captured returns what was kept, the tail cut to its exact share.
func (c *headTail) captured() captured {
	tail := c.tail
	if len(tail) > c.tailMax {
		tail = tail[len(tail)-c.tailMax:]
	}
	return captured{Head: string(c.head), Tail: string(tail), Total: c.total}
}

// captured is one output stream as kept for the model: Head and Tail are
// contiguous when Total is their length, and bracket a hole otherwise.
type captured struct {
	Head, Tail string
	Total      int64
}

// whole is a stream kept in full.
func whole(s string) captured { return captured{Head: s, Total: int64(len(s))} }

// hole is how many bytes between Head and Tail were not kept.
func (c captured) hole() int64 { return c.Total - int64(len(c.Head)+len(c.Tail)) }

// render cuts the stream to limit bytes for the model, truncateWithInfo's way;
// the elision counts bytes written, the hole included.
func (c captured) render(limit int) string {
	if c.hole() <= 0 {
		return truncateWithInfo(c.Head+c.Tail, limit)
	}
	headLimit, tailLimit := len(c.Head), len(c.Tail)
	if limit > 0 {
		headLimit = min(headLimit, limit*3/5)
		tailLimit = min(tailLimit, limit-limit*3/5)
	}
	head := trimIncompleteRune(c.Head[:backToRuneStart(c.Head, headLimit)])
	tail := skipContinuationBytes(c.Tail[forwardToRuneStart(c.Tail, len(c.Tail)-tailLimit):])
	if head == "" && tail == "" {
		return fmt.Sprintf("…[truncated, showing 0 of %d bytes]", c.Total)
	}
	return fmt.Sprintf("%s\n…[%d of %d bytes elided]\n%s", head, c.Total-int64(len(head)+len(tail)), c.Total, tail)
}

// trimRight drops trailing cutset bytes from the stream's end, for display.
func (c captured) trimRight(cutset string) captured {
	before := len(c.Head) + len(c.Tail)
	if c.Tail != "" {
		c.Tail = strings.TrimRight(c.Tail, cutset)
	} else {
		c.Head = strings.TrimRight(c.Head, cutset)
	}
	c.Total -= int64(before - len(c.Head) - len(c.Tail))
	return c
}

// trimIncompleteRune drops a trailing sequence a byte-level cut left unfinished.
func trimIncompleteRune(s string) string {
	start := len(s) - 1
	for back := 0; back < utf8.UTFMax-1 && start > 0 && !utf8.RuneStart(s[start]); back++ {
		start--
	}
	if start >= 0 && !utf8.FullRuneInString(s[start:]) {
		return s[:start]
	}
	return s
}

// skipContinuationBytes drops the continuation bytes a byte-level cut left at
// the front.
func skipContinuationBytes(s string) string {
	i := 0
	for i < len(s) && i < utf8.UTFMax && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
