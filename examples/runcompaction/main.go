// Command runcompaction demonstrates run-level compaction: a bulky tool result
// and tiny thresholds make ToolResultStrategy fold older results mid-run, while
// the session's log keeps every entry — see spec §2.5f.
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
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/models/openai"
)

type readArgs struct {
	Path string `json:"path" jsonschema_description:"File to read."`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// A deliberately bulky result: it matters for one turn and never again.
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

	// Lossless first, lossy only if that was not enough — see docs/howto/sessions.md.
	strategy := &compaction.PipelineStrategy{Strategies: []compaction.Strategy{
		&compaction.ToolResultStrategy{
			// Size is the trigger to tune in production; the group count keeps
			// the demo folding against a stub endpoint that reports two tokens.
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

	storage := session.NewInMemoryStorage("demo")
	sess := session.NewSession(storage)
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

	// The log kept everything; only the context shrank. Re-run the compactor so
	// the two counts cover the same entries, this run's last two included.
	entries, err := sess.ContextEntries(ctx, session.Cursor{})
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
