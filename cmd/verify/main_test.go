package main

import (
	"slices"
	"testing"
)

// TestSDKPackagesOneAliasPerDir guards the one mistake the tool cannot report
// itself. A misspelled directory fails loudly in goFiles, but two aliases for
// the same package are silent: checkDocGo reads <dir>/doc.go once per alias, so
// that file's links and problems are counted twice. When a package needs a
// different alias than the obvious one, rename the key — never add a second.
func TestSDKPackagesOneAliasPerDir(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for alias, dir := range sdkPackages {
		if first, dup := seen[dir]; dup {
			t.Errorf("%s has two aliases: %q and %q", dir, first, alias)
			continue
		}
		seen[dir] = alias
	}
}

// TestCheckBlock pins what a snippet's prose stripping must and must not hide.
// Running the tool cannot show it: a stripping bug only ever loses coverage, so
// the whole-repo run stays green either way.
func TestCheckBlock(t *testing.T) {
	t.Parallel()

	pkgs := map[string]*pkgInfo{
		"agents": {
			exports: map[string]bool{"Agent": true, "NewRunner": true},
			methods: map[string]map[string]bool{"Runner": {"Run": true}},
			fields:  map[string]map[string]bool{},
			ctors:   map[string]string{"NewRunner": "Runner"},
		},
	}

	tests := []struct {
		name  string
		block string
		want  []string
	}{
		{
			name:  "names that exist pass",
			block: "r := agents.NewRunner()\nr.Run(ctx)\nvar a agents.Agent\n",
		},
		{
			name:  "renamed symbol is reported",
			block: "var a agents.Gone\n",
			want:  []string{"f.md: agents.Gone does not exist"},
		},
		{
			name:  "a method the bound type lacks is reported",
			block: "r := agents.NewRunner()\nr.Nope()\n",
			want:  []string{"f.md: agents.Runner has no method Nope (called on r)"},
		},
		{
			name:  "comments are prose",
			block: "// agents.Gone was renamed to Agent\n",
		},
		{
			name:  "string literals are prose",
			block: "r := agents.NewRunner(\"agents.Gone\")\n",
		},
		{
			name:  "a URL literal does not hide the rest of its line",
			block: "r := agents.NewRunner(\"https://api.openai.com\", agents.Gone)\n",
			want:  []string{"f.md: agents.Gone does not exist"},
		},
		{
			name:  "a URL literal does not pair with a later literal",
			block: "base := \"https://api.openai.com\"\nvar a agents.Gone\nname := \"support\"\n",
			want:  []string{"f.md: agents.Gone does not exist"},
		},
		{
			name:  "an unpaired quote in a comment does not swallow the next line",
			block: "// pass the \"name option\nvar a agents.Gone\nname := \"support\"\n",
			want:  []string{"f.md: agents.Gone does not exist"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkBlock("f.md", tt.block, pkgs)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("checkBlock() = %q, want %q", got, tt.want)
			}
		})
	}
}
