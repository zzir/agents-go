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
	if pdf.Description == "" || pdf.License != "Apache-2.0" {
		t.Errorf("pdf fields = %+v", pdf)
	}
	if pdf.Metadata["author"] != "example-org" || pdf.Metadata["version"] != "1.0" {
		t.Errorf("metadata = %v", pdf.Metadata)
	}
	if len(pdf.AllowedTools) != 2 || pdf.AllowedTools[0] != "read_skill_file" {
		t.Errorf("allowed-tools = %v", pdf.AllowedTools)
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
	tool := skills.ReadFileTool(root).(*agents.FunctionTool)
	ctx := context.Background()
	tc := &agents.ToolContext{}

	out, err := tool.OnInvoke(ctx, tc, `{"path":"pdf-processing/SKILL.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.(string), "# PDF processing") {
		t.Errorf("read content missing body:\n%v", out)
	}

	// Path traversal must be rejected by os.Root.
	if _, err := tool.OnInvoke(ctx, tc, `{"path":"../../etc/passwd"}`); err == nil {
		t.Error("path traversal should be rejected")
	}
}
