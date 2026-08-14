package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/session"
)

// usageEntry is an assistant message carrying one model call's usage, which is
// how the runner records it: exactly one entry per response.
func usageEntry(t *testing.T, text string, in, out, cached int64) session.Entry {
	t.Helper()
	e := rawEntryFrom(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+quoteJSON(text)+`}],"status":"completed"}`,
		agents.Source{Type: agents.SourceModel})
	e.Usage = &session.RequestUsage{
		InputTokens:        in,
		OutputTokens:       out,
		TotalTokens:        in + out,
		InputTokensDetails: session.InputTokensDetails{CachedTokens: cached},
	}
	return e
}

// toolOutputEntry is an exec_command result with the display the runner
// projects, which is where the report reads its label and anchor from.
func toolOutputEntry(t *testing.T, callID, output string) session.Entry {
	t.Helper()
	e := rawEntryFrom(t, `{"type":"function_call_output","call_id":`+quoteJSON(callID)+`,"output":`+quoteJSON(output)+`}`,
		agents.Source{Type: agents.SourceTool})
	e.Display = &agents.ItemDisplay{Kind: agents.DisplayToolOutput, CallID: callID, ToolName: "exec_command", Output: output}
	return e
}

// The window figures come from the LAST call, the session figures from all of
// them, and the growth curve keeps one point per call.
func TestContextReportUsageIsPerCallNotCumulative(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	seed(t, s,
		userEntry(t, "first"),
		usageEntry(t, "one", 1000, 100, 0),
		userEntry(t, "second"),
		usageEntry(t, "two", 2500, 200, 1800),
	)

	rep, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	if rep.InputTokens != 2500 || rep.OutputTokens != 200 {
		t.Fatalf("window figures want 2500/200, got %d/%d", rep.InputTokens, rep.OutputTokens)
	}
	if rep.CachedTokens != 1800 {
		t.Fatalf("cached tokens want 1800, got %d", rep.CachedTokens)
	}
	if rep.SessionInputTokens != 3500 || rep.SessionOutputTokens != 300 {
		t.Fatalf("session totals want 3500/300, got %d/%d", rep.SessionInputTokens, rep.SessionOutputTokens)
	}
	if len(rep.Growth) != 2 || rep.Growth[0] != 1000 || rep.Growth[1] != 2500 {
		t.Fatalf("growth curve want [1000 2500], got %v", rep.Growth)
	}
}

// A compacted entry is out of the model's context but its call still happened:
// it leaves the conversation estimate, and keeps its usage.
func TestContextReportExcludesCompactedFromContextNotFromSpend(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	fat := toolOutputEntry(t, "call_1", strings40k())
	seed(t, s, fat, usageEntry(t, "summary", 900, 90, 0))

	before, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	// The 40k-char output dominates the estimate (~10k tokens at 4 chars/token).
	if before.ConversationTokens < 10000 {
		t.Fatalf("conversation estimate should carry the fat tool output, got %d", before.ConversationTokens)
	}

	var ids []string
	entries, err := s.load(ctx, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, e := range entries {
		if e.Display != nil && e.Display.CallID == "call_1" {
			ids = append(ids, e.ID)
		}
	}
	markCompacted(t, s, ids...)

	after, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report after compaction: %v", err)
	}
	if after.ConversationTokens >= before.ConversationTokens {
		t.Fatalf("compacted entry still counted in the conversation estimate: %d -> %d",
			before.ConversationTokens, after.ConversationTokens)
	}
	if after.SessionInputTokens != before.SessionInputTokens {
		t.Fatalf("compaction rewrote what was already spent: %d -> %d", before.SessionInputTokens, after.SessionInputTokens)
	}
}

// Only the active branch is in context: an abandoned attempt is still recorded
// and must not be counted as occupying the window.
func TestContextReportCountsOnlyTheActiveBranch(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	seed(t, s, userEntry(t, "ask"))
	abandoned := toolOutputEntry(t, "call_dead", strings40k())
	seed(t, s, abandoned)

	entries, err := s.load(ctx, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Branch back to the user message, leaving the fat tool output off-path.
	if err := session.NewSession(s).Branch(ctx, entries[0].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}

	rep, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	// The abandoned 40k-char output (~10k estimated tokens) must not count.
	if rep.ConversationTokens >= 10000 {
		t.Fatalf("off-path entry counted as context: estimate %d", rep.ConversationTokens)
	}
}

func strings40k() string {
	b := make([]byte, 40000)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// The compaction figure is what the pass compares, not a character sum: the
// last call's usage prices the history up to itself, and only the turns since
// are estimated. Reporting anything else would draw a threshold that does not
// match the one that fires.
func TestContextReportCompactionFigureMatchesTheTrigger(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	// A fat tool output the provider has already priced: the call that followed
	// it reported 12,000 input + 500 output.
	seed(t, s,
		toolOutputEntry(t, "call_1", strings40k()),
		usageEntry(t, "answer", 12000, 500, 0),
	)
	priced, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	if priced.CompactionTokens != 12500 {
		t.Fatalf("history through the last call should be its total (12500), got %d", priced.CompactionTokens)
	}

	// A turn nobody has priced yet is estimated on top.
	seed(t, s, toolOutputEntry(t, "call_2", strings40k()))
	tail, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	if tail.CompactionTokens <= priced.CompactionTokens {
		t.Fatalf("an unpriced turn must add to the figure: %d -> %d", priced.CompactionTokens, tail.CompactionTokens)
	}
}

// The report ranks and prices from the columns the append wrote, so those
// columns must be the estimator's own answer about the entry as stored — a
// drift here would silently reorder the panel and move the compaction line.
func TestAppendLiftsUsageAndEstimate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	seed(t, s,
		userEntry(t, "ask"),
		toolOutputEntry(t, "call_1", strings40k()),
		usageEntry(t, "answer", 12000, 500, 900),
	)

	var rows []entryRow
	if err := db.NewSelect().Model(&rows).Where("session_id = ?", "s1").OrderExpr("id ASC").Scan(ctx); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	est := compaction.CharEstimator{}
	priced := 0
	for _, row := range rows {
		var e session.Entry
		if err := json.Unmarshal([]byte(row.Entry), &e); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if want := est.Estimate(e); row.EstTokens != want {
			t.Fatalf("entry %s: column says %d, the estimator says %d", row.EntryID, row.EstTokens, want)
		}
		if e.Usage == nil {
			if row.Usage != "" {
				t.Fatalf("entry %s carries no usage but the column has %q", row.EntryID, row.Usage)
			}
			continue
		}
		priced++
		var u session.RequestUsage
		if err := json.Unmarshal([]byte(row.Usage), &u); err != nil {
			t.Fatalf("decode usage: %v", err)
		}
		if u != *e.Usage {
			t.Fatalf("entry %s: usage column %+v differs from the entry's %+v", row.EntryID, u, *e.Usage)
		}
	}
	if priced != 1 {
		t.Fatalf("exactly one entry per response carries usage, got %d", priced)
	}
}

// A fold newer than the last pricing invalidates it: the stale total covered
// history the fold removed, so the figure drops to the estimate of what the
// projection now sends instead of holding its pre-fold height — and the next
// priced call re-anchors it on the provider.
func TestContextReportCompactionFigureDropsAtTheFold(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	seed(t, s,
		userEntry(t, strings40k()),
		usageEntry(t, "answer", 49000, 1000, 0),
	)
	before, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	if before.CompactionTokens != 50000 {
		t.Fatalf("pre-fold figure should be the priced total, got %d", before.CompactionTokens)
	}

	ca := NewCompactionAdapter(s, &summaryFakeModel{summary: "the user pasted a large text"}, 1, 1, "", CompactionNotifier{})
	if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	folded, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report after fold: %v", err)
	}
	if folded.CompactionTokens >= 1000 {
		t.Fatalf("post-fold figure should be the estimate of tail+summary, got %d (was %d)",
			folded.CompactionTokens, before.CompactionTokens)
	}

	// The next priced call covers the folded view and re-anchors the figure.
	seed(t, s, usageEntry(t, "re-priced", 2900, 100, 0))
	rePriced, err := s.ContextReport(ctx, session.Direct("s1"))
	if err != nil {
		t.Fatalf("context report after re-pricing: %v", err)
	}
	if rePriced.CompactionTokens != 3000 {
		t.Fatalf("a usage newer than the fold anchors the figure again, got %d", rePriced.CompactionTokens)
	}
}
