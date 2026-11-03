package sandbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	agents "github.com/zzir/agents-go/agents"
)

// Whatever the chunking, the capture is the stream's first 60%, its last 40%
// and its true length — the reference is the stream itself.
func TestHeadTail_KeepsHeadTailAndTotal(t *testing.T) {
	const capacity = 100
	for _, n := range []int{0, 1, 59, 60, 61, 99, 100, 101, 139, 140, 141, 1000, 100_003} {
		full := make([]byte, n)
		for i := range full {
			full[i] = byte('A' + i%23)
		}
		for _, chunk := range []int{1, 7, 64, 1 << 20} {
			ht := newHeadTail(capacity)
			for i := 0; i < n; i += chunk {
				if _, err := ht.Write(full[i:min(n, i+chunk)]); err != nil {
					t.Fatal(err)
				}
			}
			got := ht.captured()
			wantHead, wantTail := string(full[:min(n, 60)]), string(full[min(n, 60):])
			if n > capacity {
				wantTail = string(full[n-40:])
			}
			if got.Head != wantHead || got.Tail != wantTail || got.Total != int64(n) {
				t.Errorf("n=%d chunk=%d: head %d/%d tail %d/%d total %d",
					n, chunk, len(got.Head), len(wantHead), len(got.Tail), len(wantTail), got.Total)
				if got.Tail != wantTail {
					t.Errorf("  tail = %q, want %q", got.Tail, wantTail)
				}
			}
			if hole := got.hole(); hole != int64(max(0, n-capacity)) {
				t.Errorf("n=%d chunk=%d: hole = %d, want %d", n, chunk, hole, max(0, n-capacity))
			}
		}
	}
}

// A capture with a hole renders the real tail and counts the real total: the
// elision is of what the command WROTE, not of what was kept.
func TestCaptured_RenderCountsTheHole(t *testing.T) {
	c := captured{
		Head:  "HEAD-" + strings.Repeat("h", 595),
		Tail:  strings.Repeat("t", 395) + "-TAIL",
		Total: 5000,
	}
	got := c.render(100)
	if !strings.HasPrefix(got, "HEAD-") || !strings.HasSuffix(got, "-TAIL") {
		t.Errorf("head or tail lost: %q", got)
	}
	// 60 + 40 bytes shown of 5000.
	if !strings.Contains(got, "…[4900 of 5000 bytes elided]") {
		t.Errorf("marker = %q, want the true total", got)
	}
	// No display limit: everything kept is shown, the hole alone is counted.
	if got := c.render(0); !strings.Contains(got, "…[4000 of 5000 bytes elided]") {
		t.Errorf("render(0) = %q", got)
	}
	// A limit above what was kept shows it all, too.
	if got := c.render(1 << 20); !strings.Contains(got, "…[4000 of 5000 bytes elided]") {
		t.Errorf("render(1 MiB) = %q", got)
	}
	// Without a hole the output is truncateWithInfo's, byte for byte.
	w := whole(strings.Repeat("z", 500))
	if got, want := w.render(100), truncateWithInfo(w.Head, 100); got != want {
		t.Errorf("whole render = %q, want %q", got, want)
	}
}

// The capture cuts at byte offsets; the model must still see valid UTF-8 at
// both edges of the hole, for every display limit.
func TestCaptured_RenderRespectsRuneBoundaries(t *testing.T) {
	s := strings.Repeat("世", 400) // 1200 bytes
	for _, capacity := range []int{10, 11, 12, 13, 100, 101, 102, 103} {
		ht := newHeadTail(capacity)
		_, _ = ht.Write([]byte(s))
		c := ht.captured()
		for limit := range 60 {
			if got := c.render(limit); !utf8.ValidString(got) {
				t.Fatalf("capacity %d limit %d produced invalid UTF-8: %q", capacity, limit, got)
			}
		}
	}
}

func TestCaptured_TrimRightKeepsTheTotalHonest(t *testing.T) {
	c := captured{Head: "a\n", Tail: "b\n\n", Total: 10}.trimRight("\n")
	if c.Tail != "b" || c.Total != 8 {
		t.Errorf("trimRight = %+v", c)
	}
	if w := whole("x\n\n").trimRight("\n"); w.Head != "x" || w.Total != 1 {
		t.Errorf("trimRight(whole) = %+v", w)
	}
}

// Through the streamed path, a command writing past the capture cap shows the
// model the LAST line it printed and the number of bytes it actually wrote.
func TestCodeTool_StreamedOutputKeepsTheRealTail(t *testing.T) {
	tool := CodeTool(NewLocal(), CodeToolConfig{MaxOutputBytes: 4096})
	const n = 3_000_000 // past DefaultMaxOutputBytes
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{},
		fmt.Sprintf(`{"cmd":"yes | head -c %d; echo; echo LAST-LINE","timeout_seconds":0,"workdir":""}`, n))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	if !strings.Contains(s, "LAST-LINE") {
		t.Errorf("the real tail was lost: %q", s[max(0, len(s)-200):])
	}
	if want := fmt.Sprintf("of %d bytes elided]", n+1+len("LAST-LINE\n")); !strings.Contains(s, want) {
		t.Errorf("marker does not carry the true total %q: %q", want, s[max(0, len(s)-300):])
	}
	if len(s) > 4096+512 {
		t.Errorf("model output is %d bytes, want about MaxOutputBytes", len(s))
	}
}

// A persistent shell's flood is capped the same way: the marker's total counts
// the dropped middle, and the last line is the real last line.
func TestCodeTool_SessionFloodReportsTheTrueTotal(t *testing.T) {
	body := floodBody()
	sb := &fakeTerminalSandbox{
		started: make(chan struct{}, 8),
		respond: func(cmd string) (string, int) {
			if cmd == "flood" {
				return body, 7
			}
			return "", 0
		},
	}
	tool := CodeTool(sb, CodeToolConfig{Sessions: true, MaxOutputBytes: 300})
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{},
		`{"cmd":"flood","timeout_seconds":0,"workdir":"","session_id":"s"}`)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := out.ModelOutput().(string)
	if !strings.HasPrefix(s, "exit_code: 7\noutput:\nfirst-line\n") {
		t.Errorf("head = %q", s[:min(60, len(s))])
	}
	if !strings.HasSuffix(s, "\nlast-line\n") {
		t.Errorf("tail = %q", s[max(0, len(s)-60):])
	}
	// The trailing newline is trimmed for display; everything else counts.
	if want := fmt.Sprintf("of %d bytes elided]", len(body)-1); !strings.Contains(s, want) {
		t.Errorf("marker does not carry the true total %q: %q", want, s)
	}
}
