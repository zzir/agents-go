package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func projected(t *testing.T, idx *Index) string {
	t.Helper()
	items, err := agents.ProjectEntries(idx.IncludedEntries(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, item := range items {
		raw, err := agents.MarshalInputItem(item)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func conversation(t *testing.T) []agents.SessionEntry {
	t.Helper()
	return withIDs([]agents.SessionEntry{
		item(t, `{"role":"system","content":"be brief"}`),
		user(t, "read the config"),
		call(t, "c1", "read_file"),
		output(t, "c1", strings.Repeat("config line; ", 200)),
		assistant(t, "the config sets X"),
		user(t, "now run the tests"),
		call(t, "c2", "bash"),
		output(t, "c2", strings.Repeat("test output; ", 200)),
		assistant(t, "tests pass"),
	})
}

// The cheapest compaction there is: old tool output goes, everything anyone
// actually said stays. In a coding conversation that is where nearly all the
// context lives.
func TestToolResultStrategy_FoldsOutputNotConversation(t *testing.T) {
	idx := NewIndex(conversation(t), nil)
	before := idx.ContextTokens()

	s := &ToolResultStrategy{Trigger: Always(), MinimumPreservedGroups: 2}
	changed, err := s.Compact(context.Background(), idx)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("nothing was folded")
	}
	if after := idx.ContextTokens(); after >= before {
		t.Errorf("context did not shrink: %d -> %d", before, after)
	}

	got := projected(t, idx)
	// Everything said survives.
	for _, said := range []string{"be brief", "read the config", "the config sets X", "now run the tests", "tests pass"} {
		if !strings.Contains(got, said) {
			t.Errorf("folding removed something that was said: %q\n%s", said, got)
		}
	}
	// The bulk output does not.
	if strings.Contains(got, strings.Repeat("config line; ", 20)) {
		t.Error("the folded tool output is still in context")
	}
	// But the fact that the tool ran does.
	if !strings.Contains(got, "read_file") {
		t.Errorf("folding lost the tool name; the model no longer knows it read the file:\n%s", got)
	}
}

// The most recent tool results are the ones the model is still working with.
func TestToolResultStrategy_PreservesTheRecentGroups(t *testing.T) {
	idx := NewIndex(conversation(t), nil)
	s := &ToolResultStrategy{Trigger: Always(), MinimumPreservedGroups: 4}
	if _, err := s.Compact(context.Background(), idx); err != nil {
		t.Fatal(err)
	}
	if got := projected(t, idx); !strings.Contains(got, "test output;") {
		t.Error("the most recent tool output was folded despite the preserve window")
	}
}

// Excluding is a view, not a deletion — the entries are still there to audit,
// re-run against, or fork from.
func TestStrategies_ExcludeWithoutDeleting(t *testing.T) {
	entries := conversation(t)
	idx := NewIndex(entries, nil)
	s := &TruncationStrategy{Trigger: Always(), MinimumPreservedGroups: 1}
	if _, err := s.Compact(context.Background(), idx); err != nil {
		t.Fatal(err)
	}

	var excluded int
	total := 0
	for _, g := range idx.Groups {
		total += len(g.Entries)
		if g.Excluded {
			excluded++
			if len(g.Entries) == 0 {
				t.Error("an excluded group lost its entries")
			}
			if g.ExcludeReason == "" {
				t.Error("an excluded group does not say why")
			}
		}
	}
	if excluded == 0 {
		t.Fatal("nothing was excluded")
	}
	if total != len(entries) {
		t.Errorf("index holds %d entries, started with %d — exclusion deleted some", total, len(entries))
	}
}

// Instructions apply to the whole conversation, not to the turn that carried
// them, so age is the wrong reason to drop them.
func TestTruncationStrategy_KeepsSystemContent(t *testing.T) {
	idx := NewIndex(conversation(t), nil)
	s := &TruncationStrategy{Trigger: Always(), MinimumPreservedGroups: 1}
	if _, err := s.Compact(context.Background(), idx); err != nil {
		t.Fatal(err)
	}
	if got := projected(t, idx); !strings.Contains(got, "be brief") {
		t.Errorf("truncation dropped the system instructions:\n%s", got)
	}
}

// Order is the design: fold tool output before dropping content, because the
// first is free and the second is not.
func TestPipeline_TriesTheCheapThingFirst(t *testing.T) {
	idx := NewIndex(conversation(t), nil)
	tokens := idx.ContextTokens()

	// A budget that the tool folding alone can satisfy.
	p := &PipelineStrategy{Strategies: []Strategy{
		&ToolResultStrategy{Trigger: TokensExceed(tokens / 2), Target: func(idx *Index) bool { return idx.ContextTokens() < tokens/3 }, MinimumPreservedGroups: 2},
		&TruncationStrategy{Trigger: TokensExceed(tokens), MinimumPreservedGroups: 2},
	}}
	if _, err := p.Compact(context.Background(), idx); err != nil {
		t.Fatal(err)
	}

	for _, g := range idx.Groups {
		if g.ExcludeReason == "truncation" {
			t.Error("truncation ran even though folding was enough")
		}
	}
	if got := projected(t, idx); !strings.Contains(got, "the config sets X") {
		t.Error("conversation content was dropped when folding would have sufficed")
	}
}

// Thresholds come from the model's own limits rather than numbers a caller has
// to guess.
func TestContextWindowStrategy_DerivesThresholds(t *testing.T) {
	idx := NewIndex(conversation(t), nil)
	tokens := idx.ContextTokens()

	s := &ContextWindowStrategy{
		// An input budget the conversation already exceeds by a lot.
		MaxContextWindowTokens: tokens,
		MaxOutputTokens:        tokens / 2,
		MinimumPreservedGroups: 2,
	}
	changed, err := s.Compact(context.Background(), idx)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an over-budget conversation was not compacted")
	}

	// A window that leaves no room at all is a configuration error, not a
	// silent no-op.
	bad := &ContextWindowStrategy{MaxContextWindowTokens: 100, MaxOutputTokens: 100}
	if _, err := bad.Compact(context.Background(), NewIndex(conversation(t), nil)); err == nil {
		t.Error("a context window with no input budget should be an error")
	}
}

// A trigger that never fires does nothing, and a nil one is not "always".
func TestTriggers(t *testing.T) {
	idx := NewIndex(conversation(t), nil)
	cases := map[string]struct {
		trigger Trigger
		want    bool
	}{
		"always":       {Always(), true},
		"never":        {Never(), false},
		"tokens above": {TokensExceed(0), true},
		"tokens below": {TokensExceed(1 << 30), false},
		"any":          {Any(Never(), Always()), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.trigger(idx); got != tc.want {
				t.Errorf("trigger = %v, want %v", got, tc.want)
			}
		})
	}
	if fires(nil, idx) {
		t.Error("a nil trigger must not fire")
	}
}

// A strategy whose trigger does not fire leaves the index alone.
func TestStrategies_RespectTheirTrigger(t *testing.T) {
	for name, s := range map[string]Strategy{
		"tool_result": &ToolResultStrategy{Trigger: Never()},
		"truncation":  &TruncationStrategy{Trigger: Never()},
	} {
		t.Run(name, func(t *testing.T) {
			idx := NewIndex(conversation(t), nil)
			before := idx.ContextTokens()
			changed, err := s.Compact(context.Background(), idx)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Error("a strategy ran despite its trigger not firing")
			}
			if idx.ContextTokens() != before {
				t.Error("the index changed anyway")
			}
		})
	}
}
