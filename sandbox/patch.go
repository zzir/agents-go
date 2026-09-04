package sandbox

import (
	"fmt"
	"strings"
)

// Parser and applier for Codex-style patches: a hunk is located by its context
// lines (plus an optional "@@ anchor"), never by line number. I/O is edit_tool.go's.

type patchOp int

const (
	opUpdate patchOp = iota
	opAdd
	opDelete
)

const (
	patchBegin = "*** Begin Patch"
	patchEnd   = "*** End Patch"
	updateMark = "*** Update File: "
	addMark    = "*** Add File: "
	deleteMark = "*** Delete File: "
	moveMark   = "*** Move to: "
	hunkMark   = "@@"
)

// patchHunk is one contiguous change: oldBlock (context+removed lines) is
// replaced by newBlock; an empty oldBlock is an insertion that needs an anchor.
type patchHunk struct {
	anchor   string
	oldBlock string
	newBlock string
	insert   bool
}

// fileEdit is one file's worth of the patch.
type fileEdit struct {
	op       patchOp
	path     string
	movePath string // opUpdate rename target ("" = no rename)
	addBody  string // opAdd file content
	hunks    []patchHunk
}

// rejectDuplicateSections refuses two sections on one path — every section reads the
// pre-patch file, so the result is order-dependent; Delete + Add is allowed.
func rejectDuplicateSections(edits []fileEdit) error {
	ops := make(map[string][]patchOp, len(edits))
	moves := make(map[string]bool, len(edits))
	for _, e := range edits {
		ops[e.path] = append(ops[e.path], e.op)
		// A move onto the section's own path is a plain update, not a second
		// section touching that path.
		if e.movePath != "" && e.movePath != e.path {
			moves[e.movePath] = true
			ops[e.movePath] = append(ops[e.movePath], opAdd) // a rename creates it
		}
	}
	for path, list := range ops {
		if len(list) < 2 {
			continue
		}
		// Delete + Add is the full-rewrite idiom; every other repeat is
		// order-dependent or a contradiction.
		if len(list) == 2 && !moves[path] &&
			((list[0] == opDelete && list[1] == opAdd) || (list[0] == opAdd && list[1] == opDelete)) {
			continue
		}
		return fmt.Errorf("apply_patch: file %q is touched by more than one section (%s); every section reads the file as it was BEFORE the patch, so the result would depend on the order they are applied — use one section per file (or a delete followed by an add to rewrite it whole)",
			path, describeOps(list))
	}
	return nil
}

// describeOps names a path's section kinds, so the refusal says what it saw.
func describeOps(list []patchOp) string {
	names := make([]string, 0, len(list))
	for _, o := range list {
		switch o {
		case opAdd:
			names = append(names, "add")
		case opDelete:
			names = append(names, "delete")
		case opUpdate:
			names = append(names, "update")
		}
	}
	return strings.Join(names, " + ")
}

// parsePatch parses a Codex-style patch into per-file edits. It is pure: no
// I/O, no path resolution; a structural problem is an error naming the line.
func parsePatch(patch string) ([]fileEdit, error) {
	lines := strings.Split(patch, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(trimCR(lines[i])) != patchBegin {
		i++
	}
	if i >= len(lines) {
		return nil, fmt.Errorf("apply_patch: missing %q header", patchBegin)
	}
	i++ // past Begin

	var edits []fileEdit
	for i < len(lines) {
		line := trimCR(lines[i])
		switch {
		case strings.TrimSpace(line) == patchEnd:
			if len(edits) == 0 {
				return nil, fmt.Errorf("apply_patch: patch contains no file sections")
			}
			if err := rejectDuplicateSections(edits); err != nil {
				return nil, err
			}
			return edits, nil

		case strings.HasPrefix(line, addMark):
			path := strings.TrimSpace(line[len(addMark):])
			i++
			var body []string
			for i < len(lines) {
				l := trimCR(lines[i])
				if isSectionStart(l) {
					break
				}
				body = append(body, strings.TrimPrefix(l, "+"))
				i++
			}
			edits = append(edits, fileEdit{op: opAdd, path: path, addBody: strings.Join(body, "\n")})

		case strings.HasPrefix(line, deleteMark):
			edits = append(edits, fileEdit{op: opDelete, path: strings.TrimSpace(line[len(deleteMark):])})
			i++

		case strings.HasPrefix(line, updateMark):
			e := fileEdit{op: opUpdate, path: strings.TrimSpace(line[len(updateMark):])}
			i++
			if i < len(lines) {
				if l := trimCR(lines[i]); strings.HasPrefix(l, moveMark) {
					e.movePath = strings.TrimSpace(l[len(moveMark):])
					i++
				}
			}
			hunks, ni, err := parseHunks(lines, i)
			if err != nil {
				return nil, err
			}
			if len(hunks) == 0 && e.movePath == "" {
				return nil, fmt.Errorf("apply_patch: %q has no hunks", e.path)
			}
			e.hunks = hunks
			i = ni
			edits = append(edits, e)

		case strings.TrimSpace(line) == "":
			i++ // tolerate blank lines between sections

		default:
			return nil, fmt.Errorf("apply_patch: unexpected line %q", lines[i])
		}
	}
	return nil, fmt.Errorf("apply_patch: missing %q footer", patchEnd)
}

// parseHunks reads the hunks of one Update section starting at line i, stopping
// at the next section marker. It returns the parsed hunks and the next index.
func parseHunks(lines []string, i int) ([]patchHunk, int, error) {
	var hunks []patchHunk
	for i < len(lines) {
		l := trimCR(lines[i])
		if isSectionStart(l) {
			break
		}
		if strings.TrimSpace(l) == "" {
			i++ // tolerate blank separators
			continue
		}
		var h patchHunk
		if strings.HasPrefix(l, hunkMark) {
			h.anchor = parseHunkAnchor(l)
			i++
		}
		var oldLines, newLines []string
		for i < len(lines) {
			bl := trimCR(lines[i])
			if isSectionStart(bl) || strings.HasPrefix(bl, hunkMark) {
				break
			}
			if !isHunkBodyLine(bl) {
				// An empty line is an empty context line whose leading space the model
				// stripped — unless nothing follows, then it is a separator: stop.
				if !moreBodyFollows(lines, i+1) {
					break
				}
				oldLines = append(oldLines, "")
				newLines = append(newLines, "")
				i++
				continue
			}
			switch bl[0] {
			case ' ':
				oldLines = append(oldLines, bl[1:])
				newLines = append(newLines, bl[1:])
			case '-':
				oldLines = append(oldLines, bl[1:])
			case '+':
				newLines = append(newLines, bl[1:])
			}
			i++
		}
		h.oldBlock = strings.Join(oldLines, "\n")
		h.newBlock = strings.Join(newLines, "\n")
		h.insert = len(oldLines) == 0
		if h.insert && h.anchor == "" {
			return nil, i, fmt.Errorf("apply_patch: a pure-insertion hunk needs an @@ anchor or a context line")
		}
		hunks = append(hunks, h)
	}
	return hunks, i, nil
}

// moreBodyFollows reports whether a hunk-body line appears at or after i before
// the next hunk or section: an interior blank line, not a separator.
func moreBodyFollows(lines []string, i int) bool {
	for i < len(lines) {
		l := trimCR(lines[i])
		if isSectionStart(l) || strings.HasPrefix(l, hunkMark) {
			return false
		}
		if isHunkBodyLine(l) {
			return true
		}
		i++
	}
	return false
}

// applyHunks applies hunks in order; each is located by the first whole-line
// match of oldBlock after the previous hunk's end (an anchor advances the start).
func applyHunks(content string, hunks []patchHunk) (string, error) {
	result := content
	searchFrom := 0
	for hi, h := range hunks {
		from := searchFrom
		if h.anchor != "" {
			idx := indexLines(result, h.anchor, from)
			if idx < 0 {
				idx = indexTrimmedLine(result, h.anchor, from)
			}
			if idx < 0 {
				return "", fmt.Errorf("apply_patch: hunk %d: anchor %q not found", hi+1, h.anchor)
			}
			from = idx
		}
		if h.insert {
			nl := strings.IndexByte(result[from:], '\n')
			if nl < 0 {
				// The anchor is the file's last line with no trailing newline: terminate
				// it so the block starts on its own line.
				result += "\n" + h.newBlock + "\n"
				searchFrom = len(result)
				continue
			}
			pos := from + nl + 1
			result = result[:pos] + h.newBlock + "\n" + result[pos:]
			searchFrom = pos + len(h.newBlock) + 1
			continue
		}
		start := indexLines(result, h.oldBlock, from)
		if start < 0 {
			return "", fmt.Errorf("apply_patch: hunk %d: context not found in file", hi+1)
		}
		result = result[:start] + h.newBlock + result[start+len(h.oldBlock):]
		searchFrom = start + len(h.newBlock)
	}
	return result, nil
}

// indexLines returns the first occurrence of block in s at or after from that
// spans whole lines (spec §2.7s).
func indexLines(s, block string, from int) int {
	for i := from; i <= len(s); {
		idx := strings.Index(s[i:], block)
		if idx < 0 {
			return -1
		}
		start := i + idx
		end := start + len(block)
		if (start == 0 || s[start-1] == '\n') && (end == len(s) || s[end] == '\n') {
			return start
		}
		i = start + 1
	}
	return -1
}

// indexTrimmedLine returns the start of the first whole line at or after from
// whose trimmed text equals the trimmed anchor (an anchor written unindented).
func indexTrimmedLine(s, anchor string, from int) int {
	want := strings.TrimSpace(anchor)
	if want == "" {
		return -1
	}
	for i := from; i <= len(s); {
		end := strings.IndexByte(s[i:], '\n')
		line := s[i:]
		if end >= 0 {
			line = s[i : i+end]
		}
		if strings.TrimSpace(line) == want {
			return i
		}
		if end < 0 {
			return -1
		}
		i += end + 1
	}
	return -1
}

func isSectionStart(l string) bool {
	return strings.HasPrefix(l, updateMark) ||
		strings.HasPrefix(l, addMark) ||
		strings.HasPrefix(l, deleteMark) ||
		strings.TrimSpace(l) == patchEnd
}

// isHunkBodyLine reports whether l is a context (' '), removed ('-') or added
// ('+') line.
func isHunkBodyLine(l string) bool {
	return len(l) > 0 && (l[0] == ' ' || l[0] == '-' || l[0] == '+')
}

func trimCR(l string) string { return strings.TrimSuffix(l, "\r") }

// parseHunkAnchor extracts the anchor from a "@@" header. The git form
// "@@ -a,b +c,d @@ heading" keeps only the heading; a bare range yields "".
func parseHunkAnchor(l string) string {
	rest := strings.TrimSpace(l[len(hunkMark):])
	if strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, "+") {
		if _, after, found := strings.Cut(rest, hunkMark); found {
			return strings.TrimSpace(after)
		}
		return ""
	}
	return rest
}
