package store

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
)

// summaryFakeModel is a minimal agents.Model returning a fixed summary text
// and recording the input of each GetResponse call.
type summaryFakeModel struct {
	summary string
	calls   int
	inputs  [][]agents.TResponseInputItem
}

func (m *summaryFakeModel) GetResponse(_ context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	m.calls++
	m.inputs = append(m.inputs, req.Input)
	var out responses.ResponseOutputItemUnion
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":` + mustQuote(m.summary) + `,"annotations":[]}]}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return &agents.ModelResponse{Output: []agents.TResponseOutputItem{out}, Usage: agents.NewUsage()}, nil
}

func (m *summaryFakeModel) StreamResponse(context.Context, agents.ModelRequest) iter.Seq2[*agents.TResponseStreamEvent, error] {
	return func(func(*agents.TResponseStreamEvent, error) bool) {}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// insertItemRows persists one Message per raw item JSON (empty string =
// annotation-style row without an Item), in id order.
func insertItemRows(t *testing.T, db *bun.DB, sessionID string, rawItems []string) {
	t.Helper()
	msgs := make([]Message, 0, len(rawItems))
	for _, raw := range rawItems {
		var m Message
		if raw == "" {
			m = NewAnnotationMessage(sessionID, "r1", "error", "boom")
		} else {
			m = NewItemMessageRaw(sessionID, "r1", "m1", []byte(raw))
		}
		msgs = append(msgs, m)
	}
	if _, err := db.NewInsert().Model(&msgs).Exec(context.Background()); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
}

func loadMessages(t *testing.T, db *bun.DB, sessionID string) []Message {
	t.Helper()
	var out []Message
	if err := db.NewSelect().Model(&out).
		Where("session_id = ?", sessionID).
		OrderExpr("id ASC").
		Scan(context.Background()); err != nil {
		t.Fatalf("load messages: %v", err)
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
	insertItemRows(t, db, sessionID, []string{
		userItemJSON,      // 0 — only this one may be compacted
		callItemJSON,      // 1 ┐ pair straddling the count-based split (msgSplit=2)
		outputItemJSON,    // 2 ┘
		assistantItemJSON, // 3
		userItemJSON,      // 4
		assistantItemJSON, // 5
	})

	model := &summaryFakeModel{summary: "earlier the user asked a question"}
	sa := NewSessionAdapter(db, sessionID)
	ca := NewCompactionAdapter(sa, model, 1, 4, "", CompactionNotifier{})

	spanStarted := false
	err := ca.RunCompaction(context.Background(), agents.CompactionArgs{
		StartSpan: func() *tracing.SpanHandle { spanStarted = true; return nil },
	})
	if err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	if model.calls != 1 {
		t.Fatalf("summary model calls = %d, want 1", model.calls)
	}
	if got := len(model.inputs[0]); got != 1 {
		t.Fatalf("summary input items = %d, want 1 (only the pre-pair prefix)", got)
	}
	if !spanStarted {
		t.Error("StartSpan was not called before summarizing")
	}

	msgs := loadMessages(t, db, sessionID)
	if len(msgs) != 7 {
		t.Fatalf("rows = %d, want 7 (6 originals + summary)", len(msgs))
	}
	if !msgs[0].Compacted {
		t.Error("leading user message should be compacted")
	}
	for i := 1; i < 6; i++ {
		if msgs[i].Compacted {
			t.Errorf("message %d should not be compacted (safe split must keep the call/output pair intact)", i)
		}
	}
	summary := msgs[6]
	if summary.Role != "compaction" || summary.Compacted {
		t.Errorf("summary row wrong: role=%q compacted=%v", summary.Role, summary.Compacted)
	}

	// The surviving history must still be replayable as a self-consistent
	// sequence: the call/output pair is intact after the summary.
	items, err := sa.GetItems(context.Background(), 0)
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
	insertItemRows(t, db, sessionID, []string{
		userItemJSON,      // 0 — compacted
		"",                // 1 — annotation, follows row 0 onto the compact side
		callItemJSON,      // 2 ┐ pair pulled whole into the keep side
		outputItemJSON,    // 3 ┘
		assistantItemJSON, // 4
		userItemJSON,      // 5
		assistantItemJSON, // 6
	})

	model := &summaryFakeModel{summary: "summary"}
	ca := NewCompactionAdapter(NewSessionAdapter(db, sessionID), model, 1, 4, "", CompactionNotifier{})

	// Count-based msgSplit = 3: it would strand output (row 3) from call (row 2).
	if err := ca.RunCompaction(context.Background(), agents.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}

	msgs := loadMessages(t, db, sessionID)
	wantCompacted := map[int]bool{0: true, 1: true}
	for i := range 7 {
		if msgs[i].Compacted != wantCompacted[i] {
			t.Errorf("message %d compacted = %v, want %v", i, msgs[i].Compacted, wantCompacted[i])
		}
	}
	if got := len(model.inputs[0]); got != 1 {
		t.Errorf("summary input items = %d, want 1 (annotation rows are never summarized)", got)
	}
}

// When no self-consistent non-empty prefix exists (the pair sits at the very
// start), the compaction pass is skipped entirely.
func TestCompactionAdapterSkipsWhenNoSafeSplit(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	insertItemRows(t, db, sessionID, []string{
		callItemJSON,   // 0 ┐ splitting anywhere inside is unsafe,
		outputItemJSON, // 1 ┘ and an empty prefix means nothing to summarize
		userItemJSON,   // 2
		assistantItemJSON,
	})

	model := &summaryFakeModel{summary: "should never be called"}
	ca := NewCompactionAdapter(NewSessionAdapter(db, sessionID), model, 1, 3, "", CompactionNotifier{})

	if err := ca.RunCompaction(context.Background(), agents.CompactionArgs{Force: true}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("summary model calls = %d, want 0", model.calls)
	}
	msgs := loadMessages(t, db, sessionID)
	if len(msgs) != 4 {
		t.Fatalf("rows = %d, want 4 (no summary row)", len(msgs))
	}
	for i, m := range msgs {
		if m.Compacted {
			t.Errorf("message %d compacted after a skipped pass", i)
		}
	}
}

// An unmoved split keeps the plain count-based boundary (regression guard for
// the pre-SafeSplitPoint behavior on pair-free histories).
func TestCompactionAdapterPlainSplitUnchanged(t *testing.T) {
	db := newTestDB(t)
	sessionID := NewID()
	insertItemRows(t, db, sessionID, []string{
		userItemJSON, assistantItemJSON, userItemJSON, // 0..2 — compacted
		assistantItemJSON, userItemJSON, // 3..4 — kept window
	})

	model := &summaryFakeModel{summary: "sum"}
	var started, doneBefore, doneAfter int
	ca := NewCompactionAdapter(NewSessionAdapter(db, sessionID), model, 1, 2, "", CompactionNotifier{
		OnStart: func() { started++ },
		OnDone:  func(before, after int) { doneBefore, doneAfter = before, after },
	})

	if err := ca.RunCompaction(context.Background(), agents.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 1 || len(model.inputs[0]) != 3 {
		t.Fatalf("summary call wrong: calls=%d inputs=%d, want 1 call with 3 items", model.calls, len(model.inputs[0]))
	}
	msgs := loadMessages(t, db, sessionID)
	for i := range 3 {
		if !msgs[i].Compacted {
			t.Errorf("message %d should be compacted", i)
		}
	}
	for i := 3; i < 5; i++ {
		if msgs[i].Compacted {
			t.Errorf("message %d should be kept", i)
		}
	}
	if started != 1 || doneBefore != 5 || doneAfter != 3 {
		t.Errorf("notifier saw start=%d before=%d after=%d, want 1/5/3", started, doneBefore, doneAfter)
	}
}
