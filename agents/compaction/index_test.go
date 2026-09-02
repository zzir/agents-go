package compaction

import (
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func item(t *testing.T, raw string) session.Entry {
	t.Helper()
	in, err := session.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	e, err := session.NewItemEntry(in, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func withIDs(entries []session.Entry) []session.Entry {
	for i := range entries {
		entries[i].ID = string(rune('a' + i))
		entries[i].Seq = int64(i + 1)
	}
	return entries
}

func user(t *testing.T, text string) session.Entry {
	t.Helper()
	return item(t, `{"role":"user","content":`+quote(text)+`}`)
}
func assistant(t *testing.T, text string) session.Entry {
	t.Helper()
	return item(t, `{"role":"assistant","content":`+quote(text)+`}`)
}
func call(t *testing.T, id, name string) session.Entry {
	t.Helper()
	return item(t, `{"type":"function_call","call_id":`+quote(id)+`,"name":`+quote(name)+`,"arguments":"{}"}`)
}
func output(t *testing.T, id, text string) session.Entry {
	t.Helper()
	return item(t, `{"type":"function_call_output","call_id":`+quote(id)+`,"output":`+quote(text)+`}`)
}
func reasoning(t *testing.T) session.Entry {
	t.Helper()
	return item(t, `{"type":"reasoning","id":"r1","summary":[]}`)
}
func userWithID(t *testing.T, id, text string) session.Entry {
	t.Helper()
	e := user(t, text)
	e.ID = id
	return e
}
func quote(s string) string { return `"` + s + `"` }

func kinds(idx *Index) []GroupKind {
	out := make([]GroupKind, len(idx.Groups))
	for i, g := range idx.Groups {
		out[i] = g.Kind
	}
	return out
}

// The reason grouping exists: a call and its output land in ONE group, so no
// strategy can separate them. The old design achieved this by nudging a split
// point until it stopped straddling a pair.
func TestGrouping_CallAndOutputAreOneGroup(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "what is the weather"),
		call(t, "c1", "get_weather"),
		output(t, "c1", "sunny"),
		assistant(t, "it is sunny"),
	})
	idx := NewIndex(entries, nil)

	got := kinds(idx)
	want := []GroupKind{GroupUser, GroupToolCall, GroupAssistantText}
	if len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("groups = %v, want %v", got, want)
		}
	}
	if n := len(idx.Groups[1].Entries); n != 2 {
		t.Errorf("tool-call group holds %d entries, want the call and its output", n)
	}
}

// Reasoning that precedes a tool call led to it, so they travel together.
func TestGrouping_ReasoningJoinsTheCallItPrecedes(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "q"),
		reasoning(t),
		call(t, "c1", "f"),
		output(t, "c1", "done"),
	})
	idx := NewIndex(entries, nil)

	if len(idx.Groups) != 2 || idx.Groups[1].Kind != GroupToolCall {
		t.Fatalf("groups = %v, want [user tool_call]", kinds(idx))
	}
	if n := len(idx.Groups[1].Entries); n != 3 {
		t.Errorf("tool-call group holds %d entries, want reasoning + call + output", n)
	}
}

// Reasoning NOT followed by a call is just assistant content; folding it into a
// tool group would misattribute it.
func TestGrouping_StandaloneReasoningStaysAssistant(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "q"),
		reasoning(t),
		assistant(t, "answer"),
	})
	idx := NewIndex(entries, nil)
	for _, g := range idx.Groups {
		if g.Kind == GroupToolCall {
			t.Fatalf("standalone reasoning became a tool-call group: %v", kinds(idx))
		}
	}
}

// Parallel calls in one turn belong to one group: dropping half of them would
// leave outputs with no calls.
func TestGrouping_ParallelCallsStayTogether(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "q"),
		call(t, "c1", "f"),
		call(t, "c2", "g"),
		output(t, "c1", "1"),
		output(t, "c2", "2"),
		assistant(t, "done"),
	})
	idx := NewIndex(entries, nil)

	if len(idx.Groups) != 3 {
		t.Fatalf("groups = %v, want [user tool_call assistant]", kinds(idx))
	}
	if n := len(idx.Groups[1].Entries); n != 4 {
		t.Errorf("tool-call group holds %d entries, want both calls and both outputs", n)
	}
}

// A group is never split, so whatever a strategy excludes, the remaining
// entries never contain a call without its output.
func TestGrouping_NoSplitCanStrandACall(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "q"),
		call(t, "c1", "f"),
		output(t, "c1", "1"),
		user(t, "again"),
		call(t, "c2", "g"),
		output(t, "c2", "2"),
	})
	idx := NewIndex(entries, nil)

	// Exclude every possible prefix of groups; the survivors must always pair.
	for cut := 0; cut <= len(idx.Groups); cut++ {
		for i, g := range idx.Groups {
			g.Excluded = i < cut
		}
		calls, outputs := map[string]bool{}, map[string]bool{}
		for _, e := range idx.IncludedEntries() {
			p := session.ProbeItem(e.Item)
			switch p.Type {
			case "function_call":
				calls[p.CallID] = true
			case "function_call_output":
				outputs[p.CallID] = true
			}
		}
		for id := range calls {
			if !outputs[id] {
				t.Fatalf("excluding %d groups stranded call %q without its output", cut, id)
			}
		}
		for id := range outputs {
			if !calls[id] {
				t.Fatalf("excluding %d groups left output %q without its call", cut, id)
			}
		}
	}
}

// Turns are counted from user messages, so a strategy can keep "the last two
// exchanges" rather than "the last N entries", which is not the same thing when
// one exchange ran twelve tools.
func TestGrouping_TurnsCountUserMessages(t *testing.T) {
	entries := withIDs([]session.Entry{
		item(t, `{"role":"system","content":"be brief"}`),
		user(t, "one"),
		call(t, "c1", "f"),
		output(t, "c1", "x"),
		assistant(t, "a1"),
		user(t, "two"),
		assistant(t, "a2"),
	})
	idx := NewIndex(entries, nil)

	if idx.Groups[0].Kind != GroupSystem || idx.Groups[0].TurnIndex != nil {
		t.Error("system content should belong to no turn")
	}
	if c := idx.Counts(); c.Turns != 2 {
		t.Errorf("turns = %d, want 2", c.Turns)
	}
}

// Incremental update only groups what arrived, and rebuilds when the history it
// knew is no longer a prefix — a branch switch or a compaction.
func TestIndex_UpdateIsIncrementalButRebuildsOnRewrite(t *testing.T) {
	first := withIDs([]session.Entry{user(t, "one"), assistant(t, "a")})
	idx := NewIndex(first, nil)
	if len(idx.Groups) != 2 {
		t.Fatalf("groups = %v", kinds(idx))
	}

	grown := append(append([]session.Entry(nil), first...), withIDs([]session.Entry{user(t, "two")})[0])
	grown[2].ID = "c"
	idx.Update(grown)
	if len(idx.Groups) != 3 {
		t.Fatalf("after append groups = %v, want 3", kinds(idx))
	}

	// History rewritten under it: the entries it knew are gone.
	rewritten := withIDs([]session.Entry{user(t, "fresh")})
	rewritten[0].ID = "zzz"
	idx.Update(rewritten)
	if len(idx.Groups) != 1 {
		t.Fatalf("a rewritten history should rebuild, got %v", kinds(idx))
	}
}

// The hybrid: a provider's own number is authoritative for everything it
// covers, and only what came after is estimated. Re-estimating the measured
// part would replace a fact with a guess.
func TestContextTokens_UsesProviderUsageThenEstimates(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "a long question that would estimate to something"),
		assistant(t, "an answer"),
		user(t, "follow up"),
	})
	entries[1].Usage = &agents.RequestUsage{InputTokens: 500, OutputTokens: 20, TotalTokens: 520}

	idx := NewIndex(entries, nil)
	got := idx.ContextTokens()

	// 520 measured, plus only the entry after it.
	tail := CharEstimator{}.Estimate(entries[2])
	if got != 520+tail {
		t.Errorf("ContextTokens = %d, want %d (520 measured + %d estimated tail)", got, 520+tail, tail)
	}

	// With no usage at all it estimates everything rather than reporting zero.
	for i := range entries {
		entries[i].Usage = nil
	}
	bare := NewIndex(entries, nil).ContextTokens()
	if bare == 0 {
		t.Error("a session with no usage should still be estimated, not counted as empty")
	}
	if bare >= 520 {
		t.Errorf("estimate = %d; the measured path should have been much larger", bare)
	}
}

// Entries the model never sees cost nothing in context.
func TestEstimator_NonContextEntriesAreFree(t *testing.T) {
	ann := session.NewAnnotationEntry(agents.ItemDisplay{Text: "a very long error banner indeed"}, agents.Source{})
	if got := (CharEstimator{}).Estimate(ann); got != 0 {
		t.Errorf("annotation estimated at %d tokens; it is never sent", got)
	}
}

// An image costs far more than the URL that names it.
func TestEstimator_ImagesChargeTheirRealCost(t *testing.T) {
	withImage := item(t, `{"role":"user","content":[{"type":"input_image","image_url":"http://x/y.png"}]}`)
	textOnly := user(t, "http://x/y.png")

	img := CharEstimator{}.Estimate(withImage)
	txt := CharEstimator{}.Estimate(textOnly)
	if img <= txt+100 {
		t.Errorf("image estimated at %d vs text %d; an image is not its URL's length", img, txt)
	}
}
