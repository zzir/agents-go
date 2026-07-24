package sandbox

import (
	"fmt"
	"strings"
)

// The apply_patch tool consumes a Codex-style patch: file sections delimited by
// "*** Begin Patch" / "*** End Patch", each an Update / Add / Delete on one
// file. Update hunks carry NO line numbers — a change is located by its
// surrounding context lines (and an optional "@@ anchor"), so the model never
// has to compute offsets. This file only parses and applies; the tool wrapper
// (edit_tool.go) does the sandbox I/O and atomic rollback.

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

// patchHunk is one contiguous change within an Update section. oldBlock is the
// context+removed lines that must be found in the file; newBlock is the
// context+added lines that replace them. When oldBlock is empty the hunk is a
// pure insertion and must carry an anchor to locate it.
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

// parsePatch parses a Codex-style patch into per-file edits. It is pure: no I/O,
// no path resolution. A structural problem is an error naming the offending line.
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
				// bl is a completely empty line. Models routinely strip the
				// single leading space from an empty context line, turning
				// " " into "". Treat it as an empty context line when more
				// hunk-body content still follows; otherwise it is a blank
				// separator before the next hunk or section, so stop and let
				// the caller skip it (rather than truncating this hunk and
				// silently dropping the empty line it was meant to keep).
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

// moreBodyFollows reports whether a hunk-body line (' ', '-' or '+') appears at
// or after index i before the next hunk marker or file-section boundary. It
// lets a completely blank line inside a hunk be told apart from a blank
// separator that precedes the next hunk or section: only the former still has
// body content ahead. Consecutive blank lines are skipped so a run of empty
// context lines inside a hunk is recognized as interior.
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

// applyHunks applies each hunk to content in order. A hunk is located by the
// first occurrence of its oldBlock at or after the previous hunk's end (the
// "@@ anchor", when present, first advances the search point). A hunk whose
// context can't be found is an error — the whole apply is abandoned upstream.
func applyHunks(content string, hunks []patchHunk) (string, error) {
	result := content
	searchFrom := 0
	for hi, h := range hunks {
		from := searchFrom
		if h.anchor != "" {
			idx := strings.Index(result[from:], h.anchor)
			if idx < 0 {
				return "", fmt.Errorf("apply_patch: hunk %d: anchor %q not found", hi+1, h.anchor)
			}
			from += idx
		}
		if h.insert {
			nl := strings.IndexByte(result[from:], '\n')
			if nl < 0 {
				// The anchor sits on the file's last line and that line has no
				// trailing newline. Terminate it first so the inserted block
				// starts on its own line instead of being glued onto the last
				// line's text.
				result += "\n" + h.newBlock + "\n"
				searchFrom = len(result)
				continue
			}
			pos := from + nl + 1
			result = result[:pos] + h.newBlock + "\n" + result[pos:]
			searchFrom = pos + len(h.newBlock) + 1
			continue
		}
		idx := strings.Index(result[from:], h.oldBlock)
		if idx < 0 {
			return "", fmt.Errorf("apply_patch: hunk %d: context not found in file", hi+1)
		}
		start := from + idx
		result = result[:start] + h.newBlock + result[start+len(h.oldBlock):]
		searchFrom = start + len(h.newBlock)
	}
	return result, nil
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

// parseHunkAnchor extracts the context anchor from a "@@" hunk header. Patches
// are located by context, never by line number, so the git unified-diff form
// "@@ -a,b +c,d @@ heading" is tolerated: the "-a,b +c,d" range is dropped and
// any trailing heading kept as the anchor. The Codex form "@@ heading" is used
// as-is. A bare range with no heading yields an empty anchor, so the hunk falls
// back to locating by its context lines.
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
