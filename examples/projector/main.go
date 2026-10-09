// Command projector demonstrates session.Projector: the single place that
// answers "what does the model get to read". Items and checkpoints project by
// default, nothing else; this example opts one more kind in — see spec §2.5b.
//
// Run with: go run ./examples/projector   (no API key needed)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func main() {
	ctx := context.Background()
	sess := session.NewSession(session.NewInMemoryStorage("demo"))

	if err := sess.AppendItems(ctx,
		agents.InputItemsFromText("Why did the build fail?"),
		agents.Source{Type: agents.SourceUser}); err != nil {
		log.Fatal(err)
	}

	// Terminal output the user ran by hand: recorded, not the model's to read.
	payload, err := json.Marshal(map[string]string{
		"command": "make release",
		"output":  "ld: symbol(s) not found for architecture arm64",
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sess.Append(ctx, session.Entry{
		Kind:    session.EntryKindTerminal,
		Source:  agents.Source{Type: agents.SourceUser},
		Payload: payload,
	}); err != nil {
		log.Fatal(err)
	}

	// Default projection: the terminal entry is recorded and not sent.
	report(ctx, sess, "default", nil)

	// A projector maps one kind to the items it contributes; nil suppresses the kind.
	report(ctx, sess, "with terminal output projected", map[session.EntryKind]session.Projector{
		session.EntryKindTerminal: func(e session.Entry) ([]agents.InputItem, error) {
			var t struct{ Command, Output string }
			if err := json.Unmarshal(e.Payload, &t); err != nil {
				return nil, err
			}
			// A system message: the runtime reports what happened; the user did
			// not say it.
			return agents.InputItemsFromSystemText(
				"The user ran `" + t.Command + "` in a terminal. Output:\n" + t.Output), nil
		},
	})
}

func report(ctx context.Context, sess *session.Session, label string, projectors map[session.EntryKind]session.Projector) {
	entries, err := sess.ContextEntries(ctx, session.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	items, err := session.ProjectEntries(entries, projectors)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%s — %d stored entries become %d items:\n", label, len(entries), len(items))
	for _, it := range items {
		fmt.Printf("  %s\n", session.ItemText(it))
	}
}
