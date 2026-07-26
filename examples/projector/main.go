// Command projector demonstrates EntryProjector: the single place that answers
// "what does the model get to read".
//
// A session records more than a conversation — an error banner, terminal
// output, a compaction checkpoint. That question used to be answered
// implicitly, by whatever a session happened to store, so anything worth
// keeping but not worth sending had nowhere to live.
//
// By default items and compaction checkpoints reach the model and nothing else:
// putting a terminal transcript in the model's mouth would be claiming somebody
// said it. This example opts one kind in.
//
// Run with: go run ./examples/projector   (no API key needed)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
)

func main() {
	ctx := context.Background()
	sess := agents.NewSession(agents.NewInMemoryStorage("demo"))

	if err := sess.AppendItems(ctx,
		agents.InputItemsFromText("Why did the build fail?"),
		agents.Source{Type: agents.SourceUser}); err != nil {
		log.Fatal(err)
	}

	// Terminal output the user ran by hand. Recorded, but not the model's to
	// read unless we say so.
	payload, err := json.Marshal(map[string]string{
		"command": "make release",
		"output":  "ld: symbol(s) not found for architecture arm64",
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sess.Append(ctx, agents.SessionEntry{
		Kind:    agents.EntryKindTerminal,
		Source:  agents.Source{Type: agents.SourceUser},
		Payload: payload,
	}); err != nil {
		log.Fatal(err)
	}

	// Default projection: the terminal entry is recorded and not sent.
	report(ctx, sess, "default", nil)

	// Opt it in. A projector maps one entry kind to the items it contributes;
	// mapping a kind to nil suppresses it instead.
	report(ctx, sess, "with terminal output projected", map[agents.EntryKind]agents.EntryProjector{
		agents.EntryKindTerminal: func(e agents.SessionEntry) ([]agents.TResponseInputItem, error) {
			var t struct{ Command, Output string }
			if err := json.Unmarshal(e.Payload, &t); err != nil {
				return nil, err
			}
			// A system message: the runtime is reporting what happened, and
			// attributing it to the user would put words in their mouth.
			return agents.InputItemsFromSystemText(
				"The user ran `" + t.Command + "` in a terminal. Output:\n" + t.Output), nil
		},
	})
}

func report(ctx context.Context, sess *agents.Session, label string, projectors map[agents.EntryKind]agents.EntryProjector) {
	entries, err := sess.ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	items, err := agents.ProjectEntries(entries, projectors)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%s — %d stored entries become %d items:\n", label, len(entries), len(items))
	for _, it := range items {
		fmt.Printf("  %s\n", agents.ItemText(it))
	}
}
