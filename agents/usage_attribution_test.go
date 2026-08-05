package agents

import (
	"context"
	"sync"
	"testing"
)

// Exactly one entry per response carries its usage, so summing over a session's
// entries counts each request once.
func TestUsage_OneEntryPerResponseCarriesIt(t *testing.T) {
	ctx := context.Background()
	sess := NewSession(NewInMemoryStorage("test"))
	tool := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(ctx, agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	withUsage := 0
	var summed RequestUsage
	for _, e := range entries {
		if e.Usage != nil {
			withUsage++
			addRequestUsage(&summed, e.Usage)
		}
	}
	if withUsage != 2 {
		t.Errorf("%d entries carry usage, want one per model call (2)", withUsage)
	}
	// Summing entries reproduces the run total, which is only true if nothing
	// was counted twice or missed.
	if summed.TotalTokens != res.Usage.TotalTokens {
		t.Errorf("entry usage sums to %d, run total is %d", summed.TotalTokens, res.Usage.TotalTokens)
	}
}

// The usage lands on the LAST entry of its response, which is what makes "how
// large is this conversation now" exact: everything after it needs estimating,
// nothing before it does.
func TestUsage_LandsOnTheLastEntryOfTheResponse(t *testing.T) {
	ctx := context.Background()
	sess := NewSession(NewInMemoryStorage("test"))
	tool := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	withID := func(id string, items ...OutputItem) *ModelResponse {
		r := modelResp(items...)
		r.ResponseID = id
		return r
	}
	model := &fakeModel{responses: []*ModelResponse{
		withID("resp_1", functionCallOutput(t, "probe", "c1", `{}`)),
		withID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
	if _, err := RunSync(ctx, agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	// Group by response id; the carrier must be the last of its group.
	lastOf := map[string]int{}
	for i, e := range entries {
		if e.ResponseID != "" {
			lastOf[e.ResponseID] = i
		}
	}
	for i, e := range entries {
		if e.Usage != nil && lastOf[e.ResponseID] != i {
			t.Errorf("entry %d carries usage but is not the last of response %q (that is %d)",
				i, e.ResponseID, lastOf[e.ResponseID])
		}
	}
}

// A nested run's tokens were spent on a different conversation, so they are
// attributed to the call that caused them and kept out of the context
// measurement.
func TestUsage_NestedIsAttributedToTheCall(t *testing.T) {
	inner := &Agent{Name: "inner", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "inner answer")),
	}}}
	outer := &Agent{Name: "outer", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "ask_inner", "c1", `{"input":"hi"}`)),
		modelResp(messageOutput(t, "outer answer")),
	}}}
	outer.Tools = []*Tool{inner.AsTool(AgentToolConfig{Name: "ask_inner"})}

	res, err := RunSync(context.Background(), outer, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nested := res.NestedUsage()
	if nested.TotalTokens == 0 {
		t.Fatal("the nested run's usage was not attributed to the call")
	}
	// It is part of the run total (the nested run shares the parent's usage)…
	if res.Usage.TotalTokens <= nested.TotalTokens {
		t.Errorf("run total %d does not exceed the nested %d", res.Usage.TotalTokens, nested.TotalTokens)
	}
	// …but it does not sit on the tool-output entry's Usage, where a context
	// estimate would read it as this conversation's size.
	e, err := EntryFromRunItem(findToolOutput(res.NewItems), "resp_1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Usage != nil {
		t.Error("nested usage leaked into the entry's context Usage")
	}
	if e.NestedUsage == nil || e.NestedUsage.TotalTokens == 0 {
		t.Error("the entry does not record what the nested run spent")
	}
}

// UsageByResponse answers "where did it go", which the run total cannot.
func TestUsage_ByResponse(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		{Output: []OutputItem{messageOutput(t, "one")}, ResponseID: "r1",
			Usage: &Usage{Requests: 1, InputTokens: 10, OutputTokens: 2, TotalTokens: 12}},
	}}
	agent := &Agent{Name: "a", ModelImpl: model}
	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byResp := res.UsageByResponse()
	if got := byResp["r1"].TotalTokens; got != 12 {
		t.Errorf("r1 total = %d, want 12", got)
	}
}

// --- Usage.Snapshot is a lock-guarded reader (race-clean with Add).

func TestUsage_SnapshotConcurrentWithAdd(t *testing.T) {
	u := NewUsage()
	var wg sync.WaitGroup
	const writers, adds = 8, 100
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range adds {
				u.Add(&Usage{Requests: 1, InputTokens: 2, OutputTokens: 3, TotalTokens: 5})
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_ = u.Snapshot().TotalTokens
			}
		}()
	}
	wg.Wait()

	final := u.Snapshot()
	if final.Requests != writers*adds {
		t.Errorf("Requests = %d, want %d", final.Requests, writers*adds)
	}
	if final.TotalTokens != writers*adds*5 {
		t.Errorf("TotalTokens = %d, want %d", final.TotalTokens, writers*adds*5)
	}
	if len(final.RequestUsageEntries) != writers*adds {
		t.Errorf("entries = %d, want %d", len(final.RequestUsageEntries), writers*adds)
	}
}

// --- FallbackProvider surfaces fallback-resolution errors.

type errModelProvider struct{ err error }

func (p errModelProvider) Model(string) (Model, error) { return nil, p.err }
