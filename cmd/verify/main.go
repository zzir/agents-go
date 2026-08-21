// Command verify runs the two checks that compile nothing but guard what
// readers copy first: the docs check (snippets, doc.go links and markdown
// links name things that exist — see docs.go) and the examples check (every
// example runs to completion against fake model APIs — see examples.go).
//
//	go run ./cmd/verify        # both checks
//	go run ./cmd/verify -v     # ...verbosely
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	verbose := flag.Bool("v", false, "list files checked and each example's output")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fail("%v", err)
	}

	code := verifyDocs(root, *verbose)
	if c := verifyExamples(root, *verbose); c != 0 {
		code = c
	}
	os.Exit(code)
}

// repoRoot walks up from the working directory to the module root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "docs")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no module root with a docs/ directory above %s", dir)
		}
		dir = parent
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "verify: "+format+"\n", args...)
	os.Exit(1)
}
