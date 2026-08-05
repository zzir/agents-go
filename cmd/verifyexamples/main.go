// Command verifyexamples runs every example under examples/ against fake
// model APIs (Responses and Messages, one server) and fails if one does not
// complete.
//
//	go run ./cmd/verifyexamples          # run them
//	go run ./cmd/verifyexamples -v       # ...and show each example's output
//
// Why it exists: `go build ./...` already proves the examples compile, which is
// the cheap half. It does not catch an example that compiles and then panics on
// a nil provider, loops forever, or silently prints nothing — and the examples
// are exactly what a reader copies first. A broad API change rewrites all of
// them; this is the net under that.
//
// It is deliberately NOT an assertion of model behavior. The fake answers with
// the shortest plausible response, so the only claims made here are "the
// example ran to completion, exited 0, and printed something".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
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
	os.Exit(run())
}

// run is main's body, returning the exit code rather than calling os.Exit, so
// that the signal handler installed below is released on every path out.
func run() int {
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

	// Each example runs in its own process group (see setProcessGroup), so the
	// terminal no longer delivers Ctrl-C to it along with us. Cancelling on the
	// signal ourselves is what still takes the group down, instead of leaving
	// the example behind as an orphan.
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	defer stop()

	var failed, skipped, ran int
	for _, name := range names {
		if why, ok := skips[name]; ok {
			fmt.Printf("SKIP %-16s %s\n", name, why)
			skipped++
			continue
		}
		// Each example gets its own fake, so its first turn is the one that
		// receives a tool call regardless of what ran before it. One server
		// speaks both wire protocols, keyed by path — the example's provider
		// picks its own via OPENAI_BASE_URL / ANTHROPIC_BASE_URL.
		responses := &fakeResponses{}
		messages := &fakeMessages{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/messages") {
				messages.ServeHTTP(w, r)
				return
			}
			responses.ServeHTTP(w, r)
		}))
		out, err := runExample(ctx, root, name, srv.URL)
		srv.Close()
		if ctx.Err() != nil {
			// We killed this one ourselves, so its exit status says nothing
			// about the example — and every example after it would report the
			// same. Stop here rather than manufacture a wall of failures.
			fmt.Printf("\ninterrupted while running %s\n", name)
			return 1
		}
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
		return 1
	}
	return 0
}

func runExample(ctx context.Context, root, name, baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, perExample)
	defer cancel()

	// An example with its own go.mod (one that needs a dependency the core must
	// not carry) runs from its own directory. Skipping those would quietly
	// shrink coverage exactly where the wiring is most unusual.
	dir := filepath.Join(root, "examples", name)
	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = dir
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		cmd = exec.CommandContext(ctx, "go", "run", "./examples/"+name)
		cmd.Dir = root
	}
	cmd.Env = append(os.Environ(),
		// A nested-module example is not in the root go.work, so workspace
		// mode resolves "." against the root module and cannot find the
		// package at all. CI runs with GOWORK=off and never saw it; the
		// documented local command did, and failed on examples/otel.
		"GOWORK=off",
		"OPENAI_BASE_URL="+baseURL,
		"OPENAI_API_KEY=verifyexamples",
		"ANTHROPIC_BASE_URL="+baseURL,
		"ANTHROPIC_API_KEY=verifyexamples",
		// Keep a developer's real key and org out of the run: an example that
		// ignored the BASE_URL overrides would otherwise quietly hit the real
		// API and bill for it.
		"OPENAI_ORG_ID=",
		"OPENAI_PROJECT_ID=",
		"ANTHROPIC_AUTH_TOKEN=",
	)

	// `go run` compiles and then execs the example as its own child, so killing
	// the timed-out process alone leaves the example itself running — and a
	// hanging example is precisely what this tool is here to catch. The group
	// dies together instead; WaitDelay bounds the wait in case something still
	// holds the output pipe.
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	out, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
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
