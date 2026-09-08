package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// A second reset in one run supersedes the first: the included entries carry
// one summary, the newest, and the checkpoint one fold.
func TestResetClearsEarlierStandIns(t *testing.T) {
	ctx := context.Background()
	entries := []session.Entry{
		entry("u1", "user", "first question"),
		entry("a1", "assistant", "first answer"),
		entry("u2", "user", "second question"),
	}
	for i := range entries {
		entries[i].ID += "-id"
	}
	c := New(&TruncationStrategy{Trigger: Never()}, nil)
	summaries := []string{"snapshot one", "snapshot two"}
	c.ResetSummary = func(context.Context) (string, error) { s := summaries[0]; summaries = summaries[1:]; return s, nil }

	if _, err := c.Reset(ctx, entries); err != nil {
		t.Fatal(err)
	}
	// The run goes on: the log grows past the reset, and the next reset sees
	// the whole log again, as the runner's read of the session does.
	all := append(append([]session.Entry(nil), entries...),
		entry("a2-id", "assistant", "second answer"), entry("u3-id", "user", "third question"))
	out, err := c.Reset(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range out {
		texts = append(texts, session.RenderItem(e.Item))
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "snapshot one") || !strings.Contains(joined, "snapshot two") || !strings.Contains(joined, "third question") || strings.Contains(joined, "second answer") {
		t.Fatalf("after two resets the view is %q", joined)
	}
	cp, ok, err := c.Checkpoint(all)
	if err != nil || !ok {
		t.Fatalf("checkpoint: %v %v", ok, err)
	}
	p, err := cp.CompactionPayload()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Reset || len(p.Folds) != 1 || !strings.Contains(string(p.Folds[0].Items[0]), "snapshot two") {
		t.Fatalf("checkpoint payload = %+v", p)
	}
}

func entry(id, role, text string) session.Entry {
	var items []agents.InputItem
	if role == "user" {
		items = agents.InputItemsFromText(text)
	} else {
		items = agents.InputItemsFromAssistantText(text)
	}
	e, err := session.NewItemEntry(items[0], agents.Source{Type: agents.SourceUser})
	if err != nil {
		panic(err)
	}
	e.ID = id
	return e
}

// A reset that finds nothing to fold leaves no mark behind: a later pass that
// folds something is recorded as that pass, not as a reset.
func TestResetWithNothingToFoldLeavesNoMark(t *testing.T) {
	ctx := context.Background()
	only := []session.Entry{entry("u1-id", "user", "the only question")}
	c := New(&TruncationStrategy{Trigger: Always(), MinimumPreservedGroups: 1}, nil)
	c.ResetSummary = func(context.Context) (string, error) { return "notes", nil }
	if _, err := c.Reset(ctx, only); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Checkpoint(only); err != nil {
		t.Fatal(err)
	}
	grown := append(append([]session.Entry(nil), only...),
		entry("a1-id", "assistant", "an answer"), entry("u2-id", "user", "another question"), entry("a2-id", "assistant", "another answer"))
	if _, err := c.Compact(ctx, grown); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := c.Checkpoint(grown)
	if err != nil || !ok {
		t.Fatalf("checkpoint: %v %v", ok, err)
	}
	if p, _ := cp.CompactionPayload(); p.Reset {
		t.Fatalf("a truncation pass was recorded as a reset: %+v", p)
	}
}
