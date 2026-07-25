package agents

import (
	"context"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func userItem(text string) TResponseInputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser)
}

func assistantItem(text string) TResponseInputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant)
}

func systemItem(text string) TResponseInputItem {
	return responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleSystem)
}

func summaryModel(t *testing.T, summaryText string) *fakeModel {
	t.Helper()
	return &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, summaryText)}, Usage: NewUsage()},
	}}
}

func TestSlidingWindow_NoBelowThreshold(t *testing.T) {
	sess := NewInMemorySession()
	model := summaryModel(t, "should not be called")
	sw := NewSlidingWindowSession(sess, SlidingWindowConfig{
		Threshold:    10,
		WindowSize:   5,
		SummaryModel: model,
	})

	items := []TResponseInputItem{
		userItem("a"), assistantItem("b"),
		userItem("c"), assistantItem("d"),
		userItem("e"), assistantItem("f"),
		userItem("g"), assistantItem("h"),
	}
	if err := sess.AddItems(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	if err := sw.RunCompaction(context.Background(), CompactionArgs{}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 0 {
		t.Errorf("model was called %d times, want 0", model.calls)
	}
	got, _ := sess.GetItems(context.Background(), 0)
	if len(got) != 8 {
		t.Errorf("items = %d, want 8", len(got))
	}
}

func TestSlidingWindow_CompactsAboveThreshold(t *testing.T) {
	sess := NewInMemorySession()
	model := summaryModel(t, "User discussed topics A and B")
	sw := NewSlidingWindowSession(sess, SlidingWindowConfig{
		Threshold:    4, // 7 items - 3 window = 4 overflow → triggers
		WindowSize:   3,
		SummaryModel: model,
	})

	items := []TResponseInputItem{
		userItem("a"), assistantItem("b"),
		userItem("c"), assistantItem("d"),
		userItem("e"), assistantItem("f"),
		userItem("g"),
	}
	if err := sess.AddItems(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	if err := sw.RunCompaction(context.Background(), CompactionArgs{}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1", model.calls)
	}
	got, _ := sess.GetItems(context.Background(), 0)
	if len(got) != 4 {
		t.Fatalf("items after compaction = %d, want 4 (1 summary + 3 kept)", len(got))
	}
	if !IsSingleSummary(got[:1]) {
		t.Error("first item should be a summary")
	}
}

func TestSlidingWindow_ForceIgnoresThreshold(t *testing.T) {
	sess := NewInMemorySession()
	model := summaryModel(t, "forced summary")
	sw := NewSlidingWindowSession(sess, SlidingWindowConfig{
		Threshold:    100,
		WindowSize:   2,
		SummaryModel: model,
	})

	items := []TResponseInputItem{
		userItem("a"), assistantItem("b"),
		userItem("c"), assistantItem("d"),
		userItem("e"),
	}
	if err := sess.AddItems(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	if err := sw.RunCompaction(context.Background(), CompactionArgs{Force: true}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1", model.calls)
	}
	got, _ := sess.GetItems(context.Background(), 0)
	if len(got) != 3 {
		t.Errorf("items = %d, want 3 (1 summary + 2 kept)", len(got))
	}
}

func TestSlidingWindow_SkipsSummaryOfSummary(t *testing.T) {
	sess := NewInMemorySession()
	model := summaryModel(t, "should not be called")
	sw := NewSlidingWindowSession(sess, SlidingWindowConfig{
		Threshold:    1, // 4 items - 3 window = 1 overflow → enters compaction, but skips summary-of-summary
		WindowSize:   3,
		SummaryModel: model,
	})

	items := []TResponseInputItem{
		systemItem(SummaryMarker + "\n\nPrior summary text"),
		userItem("a"), assistantItem("b"), userItem("c"),
	}
	if err := sess.AddItems(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	if err := sw.RunCompaction(context.Background(), CompactionArgs{}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 0 {
		t.Errorf("model was called %d times, want 0 (should skip summary-of-summary)", model.calls)
	}
}

func TestSlidingWindow_CustomShouldCompact(t *testing.T) {
	sess := NewInMemorySession()
	model := summaryModel(t, "custom compact")
	sw := NewSlidingWindowSession(sess, SlidingWindowConfig{
		Threshold:     100,
		WindowSize:    1,
		SummaryModel:  model,
		ShouldCompact: func(int) bool { return true },
	})

	items := []TResponseInputItem{userItem("a"), assistantItem("b")}
	if err := sess.AddItems(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	if err := sw.RunCompaction(context.Background(), CompactionArgs{}); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1", model.calls)
	}
	got, _ := sess.GetItems(context.Background(), 0)
	if len(got) != 2 {
		t.Errorf("items = %d, want 2 (1 summary + 1 kept)", len(got))
	}
}

func TestSlidingWindow_IntegrationWithRunner(t *testing.T) {
	sess := NewInMemorySession()
	runModel := &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, "hi")}, Usage: NewUsage()},
	}}
	compactModel := summaryModel(t, "compacted")
	sw := NewSlidingWindowSession(sess, SlidingWindowConfig{
		Threshold:    1,
		WindowSize:   1,
		SummaryModel: compactModel,
	})

	// Seed some history so compaction triggers.
	_ = sess.AddItems(context.Background(), []TResponseInputItem{
		userItem("old1"), assistantItem("old2"),
	})

	agent := &Agent{Name: "a", Model: "m"}
	_, err := RunSync(context.Background(), agent, "hello", RunOptions{Model: runModel, Session: sw})
	if err != nil {
		t.Fatal(err)
	}
	if compactModel.calls != 1 {
		t.Fatalf("compaction model calls = %d, want 1", compactModel.calls)
	}
}
