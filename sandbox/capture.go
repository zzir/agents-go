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
// contiguous when Total is their length, and bracket a hole otherwise. Saved
// names the file the stream was spilled to, once it was, and SavedHole counts
// the bytes that file lacks.
type captured struct {
	Head, Tail string
	Total      int64
	Saved      string
	SavedHole  int64
}

// whole is a stream kept in full.
func whole(s string) captured { return captured{Head: s, Total: int64(len(s))} }

// hole is how many bytes between Head and Tail were not kept.
func (c captured) hole() int64 { return c.Total - int64(len(c.Head)+len(c.Tail)) }

// truncated reports whether render(limit) will elide anything.
func (c captured) truncated(limit int) bool {
	return c.hole() > 0 || (limit > 0 && len(c.Head)+len(c.Tail) > limit)
}

// render cuts the stream to limit bytes for the model — the head's first 60%,
// the tail's last 40% — around a marker counting the bytes written in between,
// the hole included, and naming the file the stream was saved to.
func (c captured) render(limit int) string {
	hole := c.hole()
	if hole <= 0 && (limit <= 0 || len(c.Head)+len(c.Tail) <= limit) {
		return c.Head + c.Tail
	}
	headLimit, tailLimit := len(c.Head), len(c.Tail)
	if limit > 0 {
		headLimit, tailLimit = limit*3/5, limit-limit*3/5
	}
	var head, tail string
	if hole > 0 {
		head = trimIncompleteRune(c.Head[:backToRuneStart(c.Head, min(headLimit, len(c.Head)))])
		tail = skipContinuationBytes(c.Tail[forwardToRuneStart(c.Tail, max(0, len(c.Tail)-tailLimit)):])
	} else {
		s := c.Head + c.Tail
		head = s[:backToRuneStart(s, headLimit)]
		tail = s[forwardToRuneStart(s, len(s)-tailLimit):]
		if len(head)+len(tail) >= len(s) {
			return s
		}
	}
	var b strings.Builder
	if head == "" && tail == "" {
		fmt.Fprintf(&b, "…[truncated, showing 0 of %d bytes", c.Total)
	} else {
		fmt.Fprintf(&b, "%s\n…[%d of %d bytes elided", head, c.Total-int64(len(head)+len(tail)), c.Total)
	}
	switch {
	case c.Saved == "":
	case c.SavedHole > 0:
		fmt.Fprintf(&b, " — saved to %s, %d bytes of its middle not captured", c.Saved, c.SavedHole)
	default:
		fmt.Fprintf(&b, " — full output: %s", c.Saved)
	}
	b.WriteString("]")
	if head != "" || tail != "" {
		b.WriteString("\n")
		b.WriteString(tail)
	}
	return b.String()
}

// file is the stream as spilled: what was captured, with a line counting the
// bytes a hole took.
func (c captured) file() string {
	if hole := c.hole(); hole > 0 {
		return fmt.Sprintf("%s\n…[%d bytes not captured]\n%s", c.Head, hole, c.Tail)
	}
	return c.Head + c.Tail
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
