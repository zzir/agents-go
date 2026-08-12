package store

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// summaryFakeModel is a minimal agents.Model returning a fixed summary text
// and recording the input of each Respond call.
type summaryFakeModel struct {
	summary string
	calls   int
	inputs  [][]agents.InputItem
}

func (m *summaryFakeModel) Respond(_ context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	m.calls++
	m.inputs = append(m.inputs, req.Input)
	var out responses.ResponseOutputItemUnion
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":` + mustQuote(m.summary) + `,"annotations":[]}]}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return &agents.ModelResponse{Output: []agents.OutputItem{out}, Usage: agents.NewUsage()}, nil
}

func (m *summaryFakeModel) StreamResponse(context.Context, agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(func(*agents.ResponseStreamEvent, error) bool) {}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// summaryTranscript asserts the transcript-as-data contract — the summary
// request carries exactly ONE user message — and returns its JSON for content
// checks.
func summaryTranscript(t *testing.T, input []agents.InputItem) string {
	t.Helper()
	if len(input) != 1 {
		t.Fatalf("summary request carries %d items, want 1 transcript message", len(input))
	}
	raw, err := json.Marshal(input[0])
	if err != nil {
		t.Fatalf("marshal summary input: %v", err)
	}
	if !strings.Contains(string(raw), `"user"`) {
		t.Fatalf("transcript should ride a user message, got %s", raw)
	}
	return string(raw)
}

// insertItemRows appends one entry per raw item JSON (empty string = an
// annotation, which carries no item), in order.
func insertItemRows(t *testing.T, s *EntryStore, rawItems []string) {
	t.Helper()
	s.SetRunID("r1")
	s.SetModel("m1")
	entries := make([]session.Entry, 0, len(rawItems))
	for _, raw := range rawItems {
		if raw == "" {
			entries = append(entries, session.NewAnnotationEntry(
				agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"},
				agents.Source{Type: agents.SourceErrorHandler},
			))
			continue
		}
		entries = append(entries, rawEntry(t, raw))
	}
	seed(t, s, entries...)
}

func loadRows(t *testing.T, db *bun.DB, sessionID string) []entryRow {
	t.Helper()
	var out []entryRow
	if err := db.NewSelect().Model(&out).
		Where("session_id = ?", sessionID).
		OrderExpr("id ASC").
		Scan(context.Background()); err != nil {
		t.Fatalf("load entries: %v", err)
	}
	return out
}

const (
	userItemJSON      = `{"role":"user","content":"question"}`
	assistantItemJSON = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	callItemJSON      = `{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"}`
	outputItemJSON    = `{"type":"function_call_output","call_id":"c1","output":"sunny"}`
)

// A count-based split would separate the function_call (index 1) from its
// output (index 2). The adapter must pull the split back so the pair stays
// intact on the keep side, compacting only the leading user message — and the
// summary request must only carry the compacted prefix.
func TestCompactionAdapterKeepsCallOutputPairTogether(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	insertItemRows(t, sa, []string{
		userItemJSON,      // 0 — only this one may be compacted
		callItemJSON,      // 1 ┐ pair straddling the count-based split (msgSplit=2)
		outputItemJSON,    // 2 ┘
		assistantItemJSON, // 3
		userItemJSON,      // 4
		assistantItemJSON, // 5
	})

	model := &summaryFakeModel{summary: "earlier the user asked a question"}
	ca := NewCompactionAdapter(sa, model, 1, 4, "", CompactionNotifier{})

	spanStarted := false
	err := ca.RunCompaction(context.Background(), session.CompactionArgs{
		StartSpan: func() *tracing.SpanHandle { spanStarted = true; return nil },
	})
	if err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	if model.calls != 1 {
		t.Fatalf("summary model calls = %d, want 1", model.calls)
	}
	transcript := summaryTranscript(t, model.inputs[0])
	if !strings.Contains(transcript, "question") {
		t.Fatalf("the folded user message should be in the transcript: %s", transcript)
	}
	// The call/output pair stays on the KEEP side — out of the summary.
	if strings.Contains(transcript, "get_weather") || strings.Contains(transcript, "sunny") {
		t.Fatalf("kept entries leaked into the summary transcript: %s", transcript)
	}
	if !spanStarted {
		t.Error("StartSpan was not called before summarizing")
	}

	rows := loadRows(t, db, sessionID)
	if len(rows) != 7 {
		t.Fatalf("rows = %d, want 7 (6 originals + checkpoint)", len(rows))
	}
	if !rows[0].Compacted {
		t.Error("leading user message should be compacted")
	}
	for i := 1; i < 6; i++ {
		if rows[i].Compacted {
			t.Errorf("entry %d should not be compacted (safe split must keep the call/output pair intact)", i)
		}
	}
	summary := rows[6]
	if summary.Kind != string(session.EntryKindCompaction) || summary.Compacted {
		t.Errorf("checkpoint row wrong: kind=%q compacted=%v", summary.Kind, summary.Compacted)
	}

	// The surviving history must still be replayable as a self-consistent
	// sequence: the call/output pair is intact after the summary.
	items, err := session.NewSession(sa).ContextItems(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatalf("GetItems: %v", err)
	}
	for _, it := range items {
		if it.OfFunctionCallOutput != nil {
			found := false
			for _, other := range items {
				if other.OfFunctionCall != nil && other.OfFunctionCall.CallID == it.OfFunctionCallOutput.CallID {
					found = true
				}
			}
			if !found {
				t.Errorf("orphaned function_call_output %q in kept history", it.OfFunctionCallOutput.CallID)
			}
		}
	}
}

// Unconvertible rows (annotations) between the compacted prefix and the kept
// window follow their preceding convertible neighbor when the split is mapped
// back to message indices.
func TestCompactionAdapterMapsSplitAcrossUnconvertibleRows(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	insertItemRows(t, sa, []string{
		userItemJSON,      // 0 — compacted
		"",                // 1 — annotation, follows row 0 onto the compact side
		callItemJSON,      // 2 ┐ pair pulled whole into the keep side
		outputItemJSON,    // 3 ┘
		assistantItemJSON, // 4
		userItemJSON,      // 5
		assistantItemJSON, // 6
	})

	model := &summaryFakeModel{summary: "summary"}
	ca := NewCompactionAdapter(sa, model, 1, 4, "", CompactionNotifier{})

	// Count-based msgSplit = 3: it would strand output (row 3) from call (row 2).
	if err := ca.RunCompaction(context.Background(), session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	rows := loadRows(t, db, sessionID)
	wantCompacted := map[int]bool{0: true, 1: true}
	for i := range 7 {
		if rows[i].Compacted != wantCompacted[i] {
			t.Errorf("entry %d compacted = %v, want %v", i, rows[i].Compacted, wantCompacted[i])
		}
	}
	if boom := summaryTranscript(t, model.inputs[0]); strings.Contains(boom, "boom") {
		t.Errorf("annotation rows are never summarized, got %s", boom)
	}
}

// When no self-consistent non-empty prefix exists (the pair sits at the very
// start), the compaction pass is skipped entirely.
func TestCompactionAdapterSkipsWhenNoSafeSplit(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	insertItemRows(t, sa, []string{
		callItemJSON,   // 0 ┐ splitting anywhere inside is unsafe,
		outputItemJSON, // 1 ┘ and an empty prefix means nothing to summarize
		userItemJSON,   // 2
		assistantItemJSON,
	})

	model := &summaryFakeModel{summary: "should never be called"}
	ca := NewCompactionAdapter(sa, model, 1, 3, "", CompactionNotifier{})

	if err := ca.RunCompaction(context.Background(), session.CompactionArgs{Force: true}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("summary model calls = %d, want 0", model.calls)
	}
	rows := loadRows(t, db, sessionID)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (no checkpoint)", len(rows))
	}
	for i, r := range rows {
		if r.Compacted {
			t.Errorf("entry %d compacted after a skipped pass", i)
		}
	}
}

// An unmoved split keeps the plain count-based boundary (regression guard for
// the pre-SafeSplitPoint behavior on pair-free histories).
func TestCompactionAdapterPlainSplitUnchanged(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	insertItemRows(t, sa, []string{
		userItemJSON, assistantItemJSON, userItemJSON, // 0..2 — compacted
		assistantItemJSON, userItemJSON, // 3..4 — kept window
	})

	model := &summaryFakeModel{summary: "sum"}
	var started, doneBefore, doneAfter int
	ca := NewCompactionAdapter(sa, model, 1, 2, "", CompactionNotifier{
		OnStart: func() { started++ },
		OnDone:  func(before, after int) { doneBefore, doneAfter = before, after },
	})

	if err := ca.RunCompaction(context.Background(), session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("summary model calls = %d, want 1", model.calls)
	}
	if tr := summaryTranscript(t, model.inputs[0]); !strings.Contains(tr, "question") || !strings.Contains(tr, "answer") {
		t.Fatalf("the folded prefix should be in the transcript: %s", tr)
	}
	rows := loadRows(t, db, sessionID)
	for i := range 3 {
		if !rows[i].Compacted {
			t.Errorf("entry %d should be compacted", i)
		}
	}
	for i := 3; i < 5; i++ {
		if rows[i].Compacted {
			t.Errorf("entry %d should be kept", i)
		}
	}
	if started != 1 || doneBefore != 5 || doneAfter != 3 {
		t.Errorf("notifier saw start=%d before=%d after=%d, want 1/5/3", started, doneBefore, doneAfter)
	}
}

// persistCompaction must not insert an orphan checkpoint when the entries it
// planned to fold were deleted out from under it.
func TestPersistCompactionSkipsWhenEntriesGone(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	insertItemRows(t, sa, []string{userItemJSON, assistantItemJSON})
	rows := loadRows(t, db, sessionID)
	ids := []int64{rows[0].ID, rows[1].ID}

	summary, err := session.NewCompactionEntry(session.CompactionPayload{Summary: "sum"})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	ca := NewCompactionAdapter(sa, &summaryFakeModel{}, 1, 1, "", CompactionNotifier{})

	// Simulate a concurrent session delete: the target entries are gone.
	if _, err := db.NewDelete().Model((*entryRow)(nil)).Where("session_id = ?", sessionID).Exec(ctx); err != nil {
		t.Fatalf("delete entries: %v", err)
	}

	applied, err := ca.persistCompaction(ctx, ids, summary)
	if err != nil {
		t.Fatalf("persistCompaction: %v", err)
	}
	if applied {
		t.Fatal("compaction must not apply when target rows are gone")
	}
	if n := len(loadRows(t, db, sessionID)); n != 0 {
		t.Fatalf("orphan checkpoint inserted into a vanished session: %d rows", n)
	}

	// Positive control: with the rows present it applies and appends the checkpoint.
	insertItemRows(t, sa, []string{userItemJSON, assistantItemJSON})
	got := loadRows(t, db, sessionID)
	ids2 := []int64{got[0].ID, got[1].ID}
	summary2, err := session.NewCompactionEntry(session.CompactionPayload{Summary: "sum2"})
	if err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	applied, err = ca.persistCompaction(ctx, ids2, summary2)
	if err != nil {
		t.Fatalf("persistCompaction (positive): %v", err)
	}
	if !applied {
		t.Fatal("compaction should apply when target rows exist")
	}
	if after := loadRows(t, db, sessionID); len(after) != 3 {
		t.Fatalf("want 3 rows (2 compacted + checkpoint), got %d", len(after))
	}
}

// The checkpoint parents at the tip the fold LEFT, whichever rows the fold took.
// RunCompaction only ever folds a prefix, so its passes never move the tip;
// persistCompaction folds whatever it is handed, and a fold reaching the tip
// must carry the append point with it — otherwise the checkpoint hangs off a row
// the fold removed from the view, and the branch ends at the checkpoint with the
// kept history stranded behind it.
func TestPersistCompactionParentsTheCheckpointAtTheSurvivingTip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	insertItemRows(t, sa, []string{userItemJSON, assistantItemJSON, userItemJSON})
	rows := loadRows(t, db, sessionID)

	summary, err := session.NewCompactionEntry(session.CompactionPayload{Summary: "sum"})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	ca := NewCompactionAdapter(sa, &summaryFakeModel{}, 1, 1, "", CompactionNotifier{})
	// A fold that takes the tail, tip included, rather than a prefix.
	applied, err := ca.persistCompaction(ctx, []int64{rows[1].ID, rows[2].ID}, summary)
	if err != nil {
		t.Fatalf("persistCompaction: %v", err)
	}
	if !applied {
		t.Fatal("compaction should apply when target rows exist")
	}

	entries, err := sa.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	walk := session.PathToLeaf(entries, session.LeafOf(entries))
	if len(walk) != 2 {
		t.Fatalf("the branch walks %d of 2 entries (the survivor and the checkpoint)", len(walk))
	}
	if walk[0].ID != rows[0].EntryID {
		t.Fatalf("the branch starts at %q, want the entry the fold left, %q", walk[0].ID, rows[0].EntryID)
	}
}

// The trigger is token-based: real usage on the newest priced entry plus a
// byte estimate of everything after it. Below the threshold nothing happens —
// no summary call, no checkpoint — regardless of how many entries there are.
func TestCompactionAdapterTokenTrigger(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))
	sa.SetRunID("r1")
	sa.SetModel("m1")

	// Six tiny entries: many by count, small by tokens.
	priced := rawEntry(t, assistantItemJSON)
	priced.Usage = &agents.RequestUsage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}
	seed(t, sa,
		rawEntry(t, userItemJSON),
		priced, // the newest priced entry: history so far = 100 tokens
		rawEntry(t, userItemJSON),
		rawEntry(t, assistantItemJSON),
		rawEntry(t, userItemJSON),
		rawEntry(t, assistantItemJSON),
	)

	model := &summaryFakeModel{summary: "sum"}
	// 100 (usage) + ~4 small tails ≈ well under 10k: must not fire.
	ca := NewCompactionAdapter(sa, model, 10000, 2, "", CompactionNotifier{})
	if err := ca.RunCompaction(context.Background(), session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("below-threshold pass must not summarize; got %d calls", model.calls)
	}

	// Same history, threshold below the priced size: must fire.
	ca = NewCompactionAdapter(sa, model, 90, 2, "", CompactionNotifier{})
	if err := ca.RunCompaction(context.Background(), session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("above-threshold pass must summarize once; got %d calls", model.calls)
	}
}

// The pass sizes and folds the ACTIVE branch only: an abandoned attempt does
// not push the history over the threshold, does not leak into the summary
// request, and is never itself folded.
func TestCompactionAdapterScopedToActiveBranch(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStoreFor(db, session.Direct(sessionID))

	// A small exchange, then a fat answer the user regenerates away.
	insertItemRows(t, sa, []string{userItemJSON, assistantItemJSON})
	seed(t, sa, toolOutputEntry(t, "call_dead", strings40k()))
	entries, err := sa.load(ctx, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := session.NewSession(sa).Branch(ctx, entries[1].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}
	// The retake and more small turns — the active branch.
	insertItemRows(t, sa, []string{assistantItemJSON, userItemJSON, assistantItemJSON, userItemJSON, assistantItemJSON})

	// ~10k estimated tokens sit on the abandoned branch; the active one is tiny.
	model := &summaryFakeModel{summary: "compact"}
	ca := NewCompactionAdapter(sa, model, 1000, 2, "", CompactionNotifier{})
	if err := ca.RunCompaction(ctx, session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 0 {
		t.Fatal("an off-path entry pushed the history over the threshold")
	}

	// A firing pass folds the active prefix and leaves the abandoned attempt
	// alone: out of the summary request, not marked compacted.
	ca = NewCompactionAdapter(sa, model, 1, 2, "", CompactionNotifier{})
	if err := ca.RunCompaction(ctx, session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("summary model calls = %d, want 1", model.calls)
	}
	if strings.Contains(summaryTranscript(t, model.inputs[0]), "call_dead") {
		t.Fatal("abandoned attempt leaked into the summary request")
	}
	for _, row := range loadRows(t, db, sessionID) {
		if row.Compacted && strings.Contains(row.Entry, "call_dead") {
			t.Fatal("off-path row was folded")
		}
	}
}
