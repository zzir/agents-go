// Command verifyexamples runs every example under examples/ against a fake
// Responses API and fails if one does not complete.
//
//	go run ./cmd/verifyexamples          # run them
//	go run ./cmd/verifyexamples -v       # ...and show each example's output
//
// Why it exists: `go build ./...` already proves the examples compile, which is
// the cheap half. It does not catch an example that compiles and then panics on
// a nil provider, loops forever, or silently prints nothing — and the examples
// are exactly what a reader copies first. A broad API change rewrites all 16
// of them; this is the net under that.
//
// It is deliberately NOT an assertion of model behavior. The fake answers with
// the shortest plausible response, so the only claims made here are "the
// example ran to completion, exited 0, and printed something".
package main

import (
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// skips lists examples that need more than an OpenAI endpoint. Each one states
// what it needs, so the list can be re-litigated rather than just inherited.
var skips = map[string]string{
	"bravesearch":   "calls the live Brave Search API (BRAVE_API_KEY)",
	"fallback":      "second provider is Groq's real endpoint (GROQ_API_KEY)",
	"prompt":        "needs a server-stored prompt (OPENAI_PROMPT_ID)",
	"sandbox":       "needs a Docker daemon",
	"conversations": "needs the server-side Conversations API",
	"compaction":    "needs server-side compaction (responses.compact)",
}

// perExample bounds a single run. An example that hangs is a failure, not a
// reason for CI to sit for ten minutes.
const perExample = 60 * time.Second

func main() {
	verbose := flag.Bool("v", false, "print each example's output")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fail("locate repo root: %v", err)
	}

	names, err := exampleNames(filepath.Join(root, "examples"))
	if err != nil {
		fail("list examples: %v", err)
	}
	if len(names) == 0 {
		fail("no examples found under %s/examples", root)
	}

	var failed, skipped, ran int
	for _, name := range names {
		if why, ok := skips[name]; ok {
			fmt.Printf("SKIP %-16s %s\n", name, why)
			skipped++
			continue
		}
		// Each example gets its own fake, so its first turn is the one that
		// receives a tool call regardless of what ran before it.
		srv := httptest.NewServer(&fakeResponses{})
		out, err := runExample(root, name, srv.URL)
		srv.Close()
		if err != nil {
			fmt.Printf("FAIL %-16s %v\n", name, err)
			fmt.Printf("     ---- output ----\n%s\n", indent(out))
			failed++
			continue
		}
		ran++
		fmt.Printf("ok   %-16s\n", name)
		if *verbose {
			fmt.Print(indent(out))
		}
	}

	fmt.Printf("\n%d ran, %d skipped, %d failed\n", ran, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func runExample(root, name, baseURL string) (string, error) {
	// An example with its own go.mod (one that needs a dependency the core must
	// not carry) runs from its own directory. Skipping those would quietly
	// shrink coverage exactly where the wiring is most unusual.
	dir := filepath.Join(root, "examples", name)
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		cmd = exec.Command("go", "run", "./examples/"+name)
		cmd.Dir = root
	}
	cmd.Env = append(os.Environ(),
		"OPENAI_BASE_URL="+baseURL,
		"OPENAI_API_KEY=verifyexamples",
		// Keep a developer's real key and org out of the run: an example that
		// ignored OPENAI_BASE_URL would otherwise quietly hit the real API and
		// bill for it.
		"OPENAI_ORG_ID=",
		"OPENAI_PROJECT_ID=",
	)

	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(perExample):
		_ = cmd.Process.Kill()
		<-done
		return string(out), fmt.Errorf("timed out after %s", perExample)
	}

	if runErr != nil {
		return string(out), fmt.Errorf("exited non-zero: %w", runErr)
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("exited 0 but printed nothing")
	}
	return string(out), nil
}

// exampleNames returns the example directories that actually contain Go source.
// An empty directory is a local leftover, not an example, and must not fail the
// run.
func exampleNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, e.Name(), "*.go"))
		if len(matches) == 0 {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "     " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "verifyexamples: "+format+"\n", args...)
	os.Exit(1)
}
