package skills_test

import (
	"strings"
	"testing"

	"github.com/zzir/agents-go/skills"
)

const pdfSkill = `---
name: pdf-processing
description: Extract text and fill PDF forms. Use when working with PDFs.
license: Apache-2.0
metadata:
  author: example-org
  version: "1.0"
allowed-tools: read_skill run_code
---

# PDF processing

Step 1. ...
`

func TestParse(t *testing.T) {
	sk, err := skills.Parse([]byte(pdfSkill))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sk.Name != "pdf-processing" {
		t.Errorf("Name = %q", sk.Name)
	}
	if !strings.HasPrefix(sk.Description, "Extract text") {
		t.Errorf("Description = %q", sk.Description)
	}
}

// CRLF frontmatter parses the same as LF: a skill authored on Windows is not
// rejected over line endings.
func TestParseCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(pdfSkill, "\n", "\r\n")
	sk, err := skills.Parse([]byte(crlf))
	if err != nil {
		t.Fatalf("Parse CRLF: %v", err)
	}
	if sk.Name != "pdf-processing" {
		t.Errorf("Name = %q", sk.Name)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{"no frontmatter", "# Just markdown\n", "must start with YAML frontmatter"},
		{"unterminated", "---\nname: a\n", "unterminated frontmatter"},
		{"missing name", "---\ndescription: d\n---\nbody", "missing required field 'name'"},
		{"missing description", "---\nname: a\n---\nbody", "missing required field 'description'"},
		{"blank description", "---\nname: a\ndescription: \"  \"\n---\nbody", "missing required field 'description'"},
		{"uppercase name", "---\nname: PDF\ndescription: d\n---\nbody", "invalid name"},
		{"double hyphen", "---\nname: a--b\ndescription: d\n---\nbody", "invalid name"},
		{"leading hyphen", "---\nname: -a\ndescription: d\n---\nbody", "invalid name"},
		{"name too long", "---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\nbody", "invalid name"},
		{"description too long", "---\nname: a\ndescription: " + strings.Repeat("d", 1025) + "\n---\nbody", "exceeds 1024"},
		{"multibyte description too long", "---\nname: a\ndescription: " + strings.Repeat("描", 1025) + "\n---\nbody", "exceeds 1024"},
		{"bad yaml", "---\nname: [\n---\nbody", "parsing frontmatter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := skills.Parse([]byte(tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Parse = %v, want error containing %q", err, tc.wantSub)
			}
		})
	}
}

// The description cap counts characters, not bytes: 1024 CJK characters (3072
// bytes) are within the limit.
func TestParseMultibyteDescription(t *testing.T) {
	desc := strings.Repeat("描", 1024)
	sk, err := skills.Parse([]byte("---\nname: a\ndescription: " + desc + "\n---\nbody"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sk.Description != desc {
		t.Errorf("Description = %q", sk.Description)
	}
}

// The closing delimiter is "---" alone on its line: a quoted value whose
// continuation line starts with dashes does not end the frontmatter early.
func TestParseDashLineInsideFrontmatter(t *testing.T) {
	src := "---\nname: a\ndescription: \"first\n---- second\"\n---\nbody"
	sk, err := skills.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sk.Description != "first ---- second" {
		t.Errorf("Description = %q", sk.Description)
	}
	if _, err := skills.Parse([]byte("---\nname: a\ndescription: d\n--- \nbody")); err == nil {
		t.Fatal("Parse accepted a closing delimiter with trailing text")
	}
}

func TestRenderIndex(t *testing.T) {
	idx := skills.RenderIndex([]skills.Skill{
		{Name: "pdf", Description: "Work with PDFs."},
		{Name: "docx", Description: "Work with Word documents."},
	})
	for _, want := range []string{"# Available skills", "**pdf**: Work with PDFs.", "**docx**", "read_skill"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index missing %q:\n%s", want, idx)
		}
	}
	if got := skills.RenderIndex(nil); got != "" {
		t.Errorf("empty index = %q, want \"\"", got)
	}
}
