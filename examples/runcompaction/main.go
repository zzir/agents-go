// Command runcompaction demonstrates run-level compaction: the run is given a
// compaction.Strategy and consults it at three points — before the first model
// call, at each turn boundary, and after the run.
//
// The turn-boundary pass is the one that matters here. A single run that keeps
// calling tools can overrun its context window long before the run ends, and
// this example forces that: the tool returns a large blob, the thresholds are
// tiny, and ToolResultStrategy folds the older results away mid-run while the
// conversation itself stays intact.
//
// Nothing is deleted. The strategy marks groups excluded and leaves a one-line
// summary behind; the session's log still holds every original entry, which is
// what the counts printed at the end show.
//
// Run with: OPENAI_API_KEY=... go run ./examples/runcompaction
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/models/openai"
)

type readArgs struct {
	Path string `json:"path" jsonschema_description:"File to read."`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// A tool with a deliberately bulky result — the shape compaction exists
	// for: it mattered for one turn and never again.
	readFile := agents.NewTool("read_file", "Read a file.",
		func(_ context.Context, _ *agents.ToolContext, a readArgs) (string, error) {
			return strings.Repeat("<file contents> ", 200), nil
		})

	agent := &agents.Agent{
		Name:         "reader",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Read each file the user names, then summarize what you found."),
		Tools:        []*agents.Tool{readFile},
	}

	// Cheap and lossless first, lossy only if that was not enough. A pipeline
	// that reaches for truncation before folding tool results pays more for a
	// worse context.
	strategy := &compaction.PipelineStrategy{Strategies: []compaction.Strategy{
		&compaction.ToolResultStrategy{
			// Size is the trigger you would tune in production. The group count
			// is here so the example still demonstrates something against a
			// stub endpoint that reports a two-token response.
			Trigger: compaction.Any(
				compaction.TokensExceed(1_500),
				compaction.GroupsExceed(3),
			),
			MinimumPreservedGroups: 1, // keep the newest tool result verbatim
		},
		&compaction.TruncationStrategy{
			Trigger: compaction.TokensExceed(6_000),
		},
	}}

	storage := agents.NewInMemoryStorage("demo")
	sess := agents.NewSession(storage)
	compactor := compaction.New(strategy, nil) // nil = CharEstimator

	opts := agents.RunOptions{
		Model:        agents.ModelOptions{Provider: provider},
		Conversation: agents.ConversationOptions{Session: sess},
		// Points is zero, so all three points are active.
		Compaction: agents.CompactionOptions{Compactor: compactor},
	}

	for _, prompt := range []string{
		"Read a.txt, b.txt and c.txt.",
		"Now read d.txt and e.txt too.",
		"Which file did you read first?",
	} {
		res, err := agents.RunSync(ctx, agent, prompt, opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("\n> %s\n%s\n", prompt, res.FinalOutputString())
	}

	// The log kept everything; only the context shrank. Ask the compactor what
	// a fourth run would be given — the passes above ran before this run's last
	// two entries existed, so re-running it is what makes the two numbers
	// comparable.
	entries, err := sess.ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	kept, err := compactor.Compact(ctx, entries)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nstored entries:  %d\n", len(entries))
	fmt.Printf("context entries: %d\n", len(kept))
	for _, g := range compactor.Index().Groups {
		if g.Excluded {
			fmt.Printf("  folded %d entries (%s) -> %d replacement\n",
				len(g.Entries), g.ExcludeReason, len(g.Replacement))
		}
	}
}
