package agents

import "testing"

func TestUsageAdd(t *testing.T) {
	u := NewUsage()
	u.Add(&Usage{
		Requests: 1, InputTokens: 100, OutputTokens: 20, TotalTokens: 120,
		InputTokensDetails:  InputTokensDetails{CachedTokens: 10, CacheWriteTokens: 4},
		OutputTokensDetails: OutputTokensDetails{ReasoningTokens: 5},
	})
	u.Add(&Usage{
		Requests: 1, InputTokens: 50, OutputTokens: 10, TotalTokens: 60,
	})

	if u.Requests != 2 {
		t.Errorf("requests = %d, want 2", u.Requests)
	}
	if u.InputTokens != 150 || u.OutputTokens != 30 || u.TotalTokens != 180 {
		t.Errorf("token totals wrong: %+v", u)
	}
	if u.InputTokensDetails.CachedTokens != 10 {
		t.Errorf("cached = %d, want 10", u.InputTokensDetails.CachedTokens)
	}
	if u.InputTokensDetails.CacheWriteTokens != 4 {
		t.Errorf("cache writes = %d, want 4", u.InputTokensDetails.CacheWriteTokens)
	}
	if len(u.RequestUsageEntries) != 2 {
		t.Errorf("entries = %d, want 2 (one synthesized per request)", len(u.RequestUsageEntries))
	}
}

func TestUsageAddPreservesEntries(t *testing.T) {
	u := NewUsage()
	withEntries := &Usage{
		Requests: 1, InputTokens: 10, TotalTokens: 10,
		RequestUsageEntries: []RequestUsage{{InputTokens: 10, TotalTokens: 10}},
	}
	u.Add(withEntries)
	if len(u.RequestUsageEntries) != 1 {
		t.Fatalf("entries = %d, want 1", len(u.RequestUsageEntries))
	}
}

func TestModelSettingsResolve(t *testing.T) {
	base := &ModelSettings{
		Temperature: new(0.2),
		ToolChoice:  ToolChoiceAuto,
		MaxTokens:   new(int64(100)),
	}
	override := &ModelSettings{
		Temperature: new(0.9),
		ToolChoice:  ToolChoiceRequired,
	}
	got := base.Resolve(override)

	if *got.Temperature != 0.9 {
		t.Errorf("temperature = %v, want 0.9", *got.Temperature)
	}
	if got.ToolChoice != ToolChoiceRequired {
		t.Errorf("tool_choice = %v", got.ToolChoice)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 100 {
		t.Errorf("max_tokens should be inherited, got %v", got.MaxTokens)
	}
	// Original must be untouched.
	if *base.Temperature != 0.2 {
		t.Errorf("base mutated: temperature = %v", *base.Temperature)
	}
}

func TestModelSettingsResolveNil(t *testing.T) {
	base := &ModelSettings{Temperature: new(0.2)}
	got := base.Resolve(nil)
	if *got.Temperature != 0.2 {
		t.Errorf("temperature = %v", *got.Temperature)
	}
}

func TestInputItemsFromText(t *testing.T) {
	items := InputItemsFromText("hello")
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	if items[0].OfMessage == nil {
		t.Errorf("expected an EasyInputMessage, got %+v", items[0])
	}
}
