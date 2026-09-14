package settings_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
)

var updateDocs = flag.Bool("update", false, "rewrite the runtime-settings table in docs/reference/configuration.md")

const (
	docPath    = "../../../../docs/reference/configuration.md"
	tableBegin = "<!-- settings-table:begin — generated from internal/settings/registry.go by `make settings-doc` -->"
	tableEnd   = "<!-- settings-table:end -->"
)

// renderTable is the runtime-settings table of the configuration reference:
// one row per registry entry, in panel order.
func renderTable() string {
	var b strings.Builder
	b.WriteString("| Key | Group | Default | Meaning |\n|---|---|---|---|\n")
	for _, d := range settings.Defs() {
		def := "—"
		if d.Default != "" {
			def = "`" + d.Default + "`"
		}
		if d.Max > 0 {
			def += fmt.Sprintf(" (max `%d`)", d.Max)
		}
		meaning := d.Description
		if meaning == "" {
			meaning = d.Label
		}
		if d.Kind == settings.KindSecret {
			meaning += " (**secret**: masked on read)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", d.Key, d.Group, def, strings.ReplaceAll(meaning, "|", `\|`))
	}
	return b.String()
}

// The table between the markers is generated from the registry; a registry
// change without `make settings-doc`, or a hand edit of the table, fails here.
func TestConfigurationDocTable(t *testing.T) {
	path := filepath.FromSlash(docPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}
	doc := string(raw)
	start, end := strings.Index(doc, tableBegin), strings.Index(doc, tableEnd)
	if start < 0 || end < start {
		t.Fatalf("%s: markers %q … %q not found", docPath, tableBegin, tableEnd)
	}
	want := tableBegin + "\n" + renderTable() + tableEnd
	if got := doc[start : end+len(tableEnd)]; got == want {
		return
	}
	if *updateDocs {
		if err := os.WriteFile(path, []byte(doc[:start]+want+doc[end+len(tableEnd):]), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("%s: the runtime-settings table is stale — run `make settings-doc` in cmd/agents-server and commit the result", docPath)
}
