package sandbox

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPageLines(t *testing.T) {
	five := "one\ntwo\nthree\nfour\nfive\n"
	cases := []struct {
		name          string
		content       string
		offset, limit int
		want          string
	}{
		{"whole file", five, 0, 0, "lines 1-5 of 5\n1\tone\n2\ttwo\n3\tthree\n4\tfour\n5\tfive\n"},
		{"a page in the middle", five, 2, 2, "lines 2-3 of 5 (continue with offset=4)\n2\ttwo\n3\tthree\n"},
		{"a limit past the end", five, 4, 10, "lines 4-5 of 5\n4\tfour\n5\tfive\n"},
		{"offset past the end", five, 9, 0, "lines 0-0 of 5: offset 9 is past the end"},
		{"no trailing newline", "a\nb", 0, 0, "lines 1-2 of 2\n1\ta\n2\tb\n"},
		{"a trailing blank line counts", "a\n\n", 0, 0, "lines 1-2 of 2\n1\ta\n2\t\n"},
		{"empty file", "", 0, 0, "(empty file)"},
		{"numbers align to the widest", strings.Repeat("x\n", 10), 9, 0, "lines 9-10 of 10\n 9\tx\n10\tx\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageLines(tc.content, tc.offset, tc.limit, 8192); got != tc.want {
				t.Fatalf("pageLines = %q, want %q", got, tc.want)
			}
		})
	}
}

// The byte budget cuts a page on a line boundary and the header says where to
// resume; a single line over budget still comes back, truncated rune-safely.
func TestPageLinesStopsAtTheOutputLimit(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&sb, "line %03d 中文字符\n", i)
	}
	got := pageLines(sb.String(), 0, 0, 1024)
	if len(got) > 1024 {
		t.Fatalf("page is %d bytes, over the 1024 limit", len(got))
	}
	header, body, _ := strings.Cut(got, "\n")
	if !strings.HasPrefix(header, "lines 1-") || !strings.Contains(header, "stopped at the output limit; continue with offset=") {
		t.Fatalf("header = %q", header)
	}
	if !utf8.ValidString(body) || !strings.HasSuffix(body, "\n") {
		t.Fatalf("body is cut inside a line or a rune: %q", body[len(body)-20:])
	}
	var first, last, total, next int
	if _, err := fmt.Sscanf(header, "lines %d-%d of %d (stopped at the output limit; continue with offset=%d)", &first, &last, &total, &next); err != nil {
		t.Fatalf("header %q: %v", header, err)
	}
	if first != 1 || total != 200 || next != last+1 || strings.Count(body, "\n") != last {
		t.Fatalf("header %q does not match the %d lines returned", header, strings.Count(body, "\n"))
	}
	// Resuming from next reaches the end eventually.
	tail := pageLines(sb.String(), next, 0, 1024)
	if !strings.HasPrefix(tail, fmt.Sprintf("lines %d-", next)) {
		t.Fatalf("resume page = %q", strings.SplitN(tail, "\n", 2)[0])
	}

	long := strings.Repeat("字", 2000) + "\nshort\n"
	got = pageLines(long, 0, 0, 1024)
	header, body, _ = strings.Cut(got, "\n")
	if header != "lines 1-1 of 2 (stopped at the output limit; continue with offset=2)" {
		t.Fatalf("header = %q", header)
	}
	if !utf8.ValidString(body) || !strings.Contains(body, "elided") {
		t.Fatalf("an over-budget line is returned cut, rune-safe: %q", body)
	}
}

func TestReadFileTool_PagesAndRefusesNegatives(t *testing.T) {
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	tool := ReadFileTool(sb, FileToolConfig{})
	if out := invokeTool(t, WriteFileTool(sb, FileToolConfig{}), `{"path":"f.txt","content":"a\nb\nc\n"}`); !strings.Contains(out, "wrote") {
		t.Fatal(out)
	}
	if got := invokeTool(t, tool, `{"path":"f.txt","offset":2,"limit":1}`); got != "lines 2-2 of 3 (continue with offset=3)\n2\tb\n" {
		t.Fatalf("page = %q", got)
	}
	if got := invokeTool(t, tool, `{"path":"f.txt","offset":-1,"limit":0}`); !strings.HasPrefix(got, "error: read f.txt: offset and limit must not be negative") {
		t.Fatalf("negative offset = %q", got)
	}
}
