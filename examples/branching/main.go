// Command branching demonstrates that a session is a tree, not a list.
//
// "Try that again differently" is the natural shape of a conversation, and a
// list cannot express it: the alternative is deleting the attempt you did not
// like. A branch keeps it, and the two branches share everything before the
// point they diverge rather than duplicating it.
//
// The switch is itself an APPEND — a leaf entry — so the history records that a
// branch was abandoned and when, and the current leaf is derived by folding the
// log rather than stored beside it where it could disagree after a crash.
//
// Run with: go run ./examples/branching   (no API key needed)
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
)

func main() {
	ctx := context.Background()
	sess := agents.NewSession(agents.NewInMemoryStorage("demo"))

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
	entries, err := sess.ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	forkPoint := entries[len(entries)-1].ID

	say(agents.SourceUser, "Make it rain the whole time.")
	say(agents.SourceModel, "Then: Nishiki Market and the Railway Museum.")
	show(ctx, sess, "after the first branch")

	// Branch from the earlier point. Everything before it is shared; the two
	// answers above are not lost, they are simply not on this branch.
	if err := sess.Branch(ctx, forkPoint); err != nil {
		log.Fatal(err)
	}
	say(agents.SourceUser, "Actually, make it a food trip.")
	say(agents.SourceModel, "Then: Nishiki Market, then kaiseki in Gion.")
	show(ctx, sess, "after branching from the plan")

	// The abandoned branch is still in the log — nothing was deleted.
	all, err := sess.Entries(ctx, agents.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nentries stored:  %d  (nothing was deleted)\n", len(all))
	ctxEntries, err := sess.ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("entries in context: %d  (only this branch)\n", len(ctxEntries))
}

func show(ctx context.Context, sess *agents.Session, label string) {
	items, err := sess.ContextItems(ctx, agents.Cursor{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%s — the model would be sent %d items:\n", label, len(items))
	for _, it := range items {
		fmt.Printf("  %s\n", firstLine(agents.ItemText(it)))
	}
}

func firstLine(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
