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

// TestReadFileToolTruncationMarker pins that a read capped at 256 KiB ends
// with an explicit truncation marker, so the model never mistakes a partial
// file for the whole thing, and that files under the cap come back verbatim.
func TestReadFileToolTruncationMarker(t *testing.T) {
	const readCap = 256 << 10 // mirrors the package's unexported maxSkillFileBytes

	root := t.TempDir()
	writeSkill(t, root, "pdf-processing", pdfSkill)
	refDir := filepath.Join(root, "pdf-processing", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", readCap+512)
	if err := os.WriteFile(filepath.Join(refDir, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := skills.ReadFileTool(root).(*agents.FunctionTool)
	ctx := context.Background()
	tc := &agents.ToolContext{}

	out, err := tool.OnInvoke(ctx, tc, `{"path":"pdf-processing/references/big.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	s := out.ModelOutput().(string)
	if !strings.Contains(s, "[... truncated: file is") {
		t.Errorf("capped read must end with a truncation marker; tail = %q", s[len(s)-120:])
	}
	if !strings.HasPrefix(s, "xxxx") {
		t.Errorf("content prefix lost: %q", s[:8])
	}
	if len(s) < readCap || len(s) > readCap+256 {
		t.Errorf("output length = %d, want the %d-byte cap plus a short marker", len(s), readCap)
	}

	// A file under the cap must come back verbatim, with no marker.
	small, err := tool.OnInvoke(ctx, tc, `{"path":"pdf-processing/SKILL.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(small.ModelOutput().(string), "truncated") {
		t.Error("a file under the cap must not carry a truncation marker")
	}
}
