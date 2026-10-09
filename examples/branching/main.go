// Command branching demonstrates that a session is a tree, not a list: a
// branch keeps the attempt it abandons, shares everything before the fork, and
// the switch is itself an appended leaf entry — see spec §2.5d.
//
// Run with: go run ./examples/branching   (no API key needed)
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func main() {
	ctx := context.Background()
	sess := session.NewSession(session.NewInMemoryStorage("demo"))

	say := func(src agents.SourceType, text string) {
		items := agents.InputItemsFromText(text)
		if src != agents.SourceUser {
			items = agents.InputItemsFromAssistantText(text)
		}
		if err := sess.AppendItems(ctx, items, agents.Source{Type: src}); err != nil {
			log.Fatal(err)
		}
	}

	say(agents.SourceUser, "Plan a weekend in Kyoto.")
	say(agents.SourceModel, "Day 1: Fushimi Inari. Day 2: Arashiyama.")

	// Remember where we are, then keep going down this branch.
	entries, err := sess.ContextEntries(ctx, session.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	forkPoint := entries[len(entries)-1].ID

	say(agents.SourceUser, "Make it rain the whole time.")
	say(agents.SourceModel, "Then: Nishiki Market and the Railway Museum.")
	show(ctx, sess, "after the first branch")

	// Branch from the earlier point; the two answers above stay stored, off
	// this branch.
	if err := sess.Branch(ctx, forkPoint); err != nil {
		log.Fatal(err)
	}
	say(agents.SourceUser, "Actually, make it a food trip.")
	say(agents.SourceModel, "Then: Nishiki Market, then kaiseki in Gion.")
	show(ctx, sess, "after branching from the plan")

	// The abandoned branch is still in the log — nothing was deleted.
	all, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nentries stored:  %d  (nothing was deleted)\n", len(all))
	ctxEntries, err := sess.ContextEntries(ctx, session.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("entries in context: %d  (only this branch)\n", len(ctxEntries))
}

func show(ctx context.Context, sess *session.Session, label string) {
	items, err := sess.ContextItems(ctx, session.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%s — the model would be sent %d items:\n", label, len(items))
	for _, it := range items {
		fmt.Printf("  %s\n", firstLine(session.ItemText(it)))
	}
}

func firstLine(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
