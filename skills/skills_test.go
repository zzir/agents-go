package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/skills"
)

// writeSkill creates rootDir/<dir>/SKILL.md with the given content.
func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const pdfSkill = `---
name: pdf-processing
description: Extract text and fill PDF forms. Use when working with PDFs.
license: Apache-2.0
metadata:
  author: example-org
  version: "1.0"
allowed-tools: read_skill_file run_code
---

# PDF processing

Step 1. ...
`

func TestLoad(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf-processing", pdfSkill)
	writeSkill(t, root, "code-review", "---\nname: code-review\ndescription: Review code changes.\n---\nbody\n")
	// A directory with no SKILL.md is skipped.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := skills.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d skills, want 2", len(got))
	}
	// Sorted by name: code-review, pdf-processing.
	if got[0].Name != "code-review" || got[1].Name != "pdf-processing" {
		t.Fatalf("order = %s, %s", got[0].Name, got[1].Name)
	}
	pdf := got[1]
	if pdf.Description == "" {
		t.Errorf("pdf fields = %+v", pdf)
	}
	if pdf.Path != filepath.Join("pdf-processing", "SKILL.md") {
		t.Errorf("path = %q", pdf.Path)
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	cases := map[string]string{
		"missing-name":   "---\ndescription: x\n---\n",
		"bad-name":       "---\nname: Bad_Name\ndescription: x\n---\n",
		"name-mismatch":  "---\nname: other-name\ndescription: x\n---\n",
		"missing-desc":   "---\nname: missing-desc\n---\n",
		"no-frontmatter": "just markdown, no frontmatter\n",
	}
	for dir, content := range cases {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			writeSkill(t, root, dir, content)
			if _, err := skills.Load(root); err == nil {
				t.Errorf("expected error for %s", dir)
			}
		})
	}
}

func TestLoadRecursive(t *testing.T) {
	root := t.TempDir()
	// A loose skill directly under root, same as Load would find.
	writeSkill(t, root, "code-review", "---\nname: code-review\ndescription: Review code changes.\n---\nbody\n")
	// A cloned multi-skill repo, one level deeper.
	writeSkill(t, root, filepath.Join("some-repo", "docx"), "---\nname: docx\ndescription: Edit Word documents.\n---\nbody\n")
	writeSkill(t, root, filepath.Join("some-repo", "pdf-processing"), pdfSkill)
	// A skill nested three levels deep — beyond what a naive two-pass scan would find.
	writeSkill(t, root, filepath.Join("outer", "inner", "deep-skill"), "---\nname: deep-skill\ndescription: Buried three levels down.\n---\nbody\n")
	// A directory with no SKILL.md anywhere under it is skipped, not an error.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := skills.LoadRecursive(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("loaded %d skills, want 4: %+v", len(got), got)
	}
	byPath := make(map[string]skills.Skill, len(got))
	for _, sk := range got {
		byPath[sk.Path] = sk
	}

	loose, ok := byPath["code-review/SKILL.md"]
	if !ok {
		t.Fatalf("missing loose skill, got paths %v", pathsOf(got))
	}
	if loose.Dir != "code-review" {
		t.Errorf("loose skill Dir = %q, want %q", loose.Dir, "code-review")
	}

	docx, ok := byPath["some-repo/docx/SKILL.md"]
	if !ok {
		t.Fatalf("missing nested repo skill, got paths %v", pathsOf(got))
	}
	if docx.Dir != "some-repo/docx" {
		t.Errorf("nested skill Dir = %q, want %q", docx.Dir, "some-repo/docx")
	}

	deep, ok := byPath["outer/inner/deep-skill/SKILL.md"]
	if !ok {
		t.Fatalf("missing three-levels-deep skill, got paths %v", pathsOf(got))
	}
	if deep.Name != "deep-skill" {
		t.Errorf("deep skill Name = %q, want %q", deep.Name, "deep-skill")
	}

	// Sorted by Path, not Name, so same-named skills from different repos don't collide.
	for i := 1; i < len(got); i++ {
		if got[i-1].Path >= got[i].Path {
			t.Errorf("results not sorted by Path: %q before %q", got[i-1].Path, got[i].Path)
		}
	}
}

func pathsOf(got []skills.Skill) []string {
	out := make([]string, len(got))
	for i, sk := range got {
		out[i] = sk.Path
	}
	return out
}

func TestRenderIndex(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf-processing", pdfSkill)
	got, _ := skills.Load(root)

	idx := skills.RenderIndex(got)
	for _, want := range []string{"pdf-processing", "Extract text", "pdf-processing/SKILL.md", "read_skill_file"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index missing %q\n%s", want, idx)
		}
	}
	if skills.RenderIndex(nil) != "" {
		t.Error("empty skills should render empty index")
	}
}

func TestReadFileTool(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf-processing", pdfSkill)
	tool := skills.ReadFileTool(root)
	ctx := context.Background()
	tc := &agents.ToolContext{}

	out, err := tool.OnInvoke(ctx, tc, `{"path":"pdf-processing/SKILL.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.ModelOutput().(string), "# PDF processing") {
		t.Errorf("read content missing body:\n%v", out)
	}

	// Path traversal must be rejected by os.Root.
	if _, err := tool.OnInvoke(ctx, tc, `{"path":"../../etc/passwd"}`); err == nil {
		t.Error("path traversal should be rejected")
	}
}
