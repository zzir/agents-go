package agents

import "testing"

func TestResolveReplacesExtrasWholesale(t *testing.T) {
	base := &ModelSettings{
		ExtraHeaders: map[string]string{"a": "1", "b": "2"},
		ExtraQuery:   map[string]string{"q": "base"},
		ExtraBody:    map[string]any{"x": 1},
	}
	override := &ModelSettings{
		ExtraHeaders: map[string]string{"b": "9"},
		ExtraQuery:   map[string]string{"r": "over"},
		ExtraBody:    map[string]any{"y": 2},
	}
	got := base.Resolve(override)

	if len(got.ExtraHeaders) != 1 || got.ExtraHeaders["b"] != "9" {
		t.Errorf("ExtraHeaders = %v, want wholesale replacement {b:9}", got.ExtraHeaders)
	}
	if _, ok := got.ExtraHeaders["a"]; ok {
		t.Errorf("ExtraHeaders retained base key a (per-key merge): %v", got.ExtraHeaders)
	}
	if len(got.ExtraQuery) != 1 || got.ExtraQuery["r"] != "over" {
		t.Errorf("ExtraQuery = %v, want wholesale replacement {r:over}", got.ExtraQuery)
	}
	if len(got.ExtraBody) != 1 || got.ExtraBody["y"] != 2 {
		t.Errorf("ExtraBody = %v, want wholesale replacement {y:2}", got.ExtraBody)
	}
}

func TestResolveKeepsBaseExtrasWhenOverrideUnset(t *testing.T) {
	base := &ModelSettings{ExtraHeaders: map[string]string{"a": "1"}}
	got := base.Resolve(&ModelSettings{Temperature: Ptr(0.5)})
	if got.ExtraHeaders["a"] != "1" {
		t.Errorf("ExtraHeaders = %v, want base retained when override unset", got.ExtraHeaders)
	}
}

func TestResolvePromptCacheKey(t *testing.T) {
	base := &ModelSettings{PromptCacheKey: "base"}

	if got := base.Resolve(&ModelSettings{PromptCacheKey: "over"}); got.PromptCacheKey != "over" {
		t.Errorf("PromptCacheKey = %q, want override to win", got.PromptCacheKey)
	}
	if got := base.Resolve(&ModelSettings{}); got.PromptCacheKey != "base" {
		t.Errorf("PromptCacheKey = %q, want base retained when override empty", got.PromptCacheKey)
	}
}

func TestResolvePromptCacheOptions(t *testing.T) {
	base := &ModelSettings{PromptCacheOptions: &PromptCacheOptions{Mode: PromptCacheModeImplicit}}

	over := &PromptCacheOptions{Mode: PromptCacheModeExplicit, TTL: "30m"}
	if got := base.Resolve(&ModelSettings{PromptCacheOptions: over}); got.PromptCacheOptions != over {
		t.Errorf("PromptCacheOptions = %+v, want override to win", got.PromptCacheOptions)
	}
	if got := base.Resolve(&ModelSettings{}); got.PromptCacheOptions == nil || got.PromptCacheOptions.Mode != PromptCacheModeImplicit {
		t.Errorf("PromptCacheOptions = %+v, want base retained when override unset", got.PromptCacheOptions)
	}
}

func TestResolveContextManagement(t *testing.T) {
	cm := []ContextManagement{{Type: "compaction", CompactThreshold: Ptr[int64](200000)}}
	got := (&ModelSettings{}).Resolve(&ModelSettings{ContextManagement: cm})
	if len(got.ContextManagement) != 1 || got.ContextManagement[0].Type != "compaction" {
		t.Fatalf("ContextManagement = %v, want single compaction entry", got.ContextManagement)
	}
	if got.ContextManagement[0].CompactThreshold == nil || *got.ContextManagement[0].CompactThreshold != 200000 {
		t.Errorf("CompactThreshold = %v, want 200000", got.ContextManagement[0].CompactThreshold)
	}

	base := &ModelSettings{ContextManagement: cm}
	if got := base.Resolve(&ModelSettings{}); len(got.ContextManagement) != 1 {
		t.Errorf("ContextManagement dropped when override unset: %v", got.ContextManagement)
	}
}
