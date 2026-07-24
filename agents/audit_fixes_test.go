package agents

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// --- NewFunctionTool non-strict validation actually relaxes required keys.

func TestNewFunctionTool_NonStrictAllowsOmittedOptional(t *testing.T) {
	type args struct {
		City  string `json:"city" jsonschema:"the city"`
		Units string `json:"units,omitempty"`
	}
	tool := NewFunctionTool("get_weather", "",
		func(ctx context.Context, tc *ToolContext, a args) (string, error) {
			return a.City, nil
		})
	tc := &ToolContext{RunContext: NewRunContext(nil)}

	// Strict (default): the strict schema marks every field required, so omitting
	// the optional "units" is rejected as a missing-required-key error.
	if _, err := tool.OnInvoke(context.Background(), tc, `{"city":"Paris"}`); err == nil {
		t.Fatal("strict: expected missing-required-key error for omitted optional field")
	}

	// Non-strict: omitting the ",omitempty" field must now be accepted — before
	// the fix the closure kept validating against the all-required strict schema
	// and the relaxed tool was unusable.
	tool.Strict = false
	out, err := tool.OnInvoke(context.Background(), tc, `{"city":"Paris"}`)
	if err != nil {
		t.Fatalf("non-strict: omitted optional field should be accepted, got %v", err)
	}
	if out != "Paris" {
		t.Errorf("out = %v, want Paris", out)
	}

	// A genuinely required field is still enforced in non-strict mode.
	if _, err := tool.OnInvoke(context.Background(), tc, `{}`); err == nil {
		t.Fatal("non-strict: required field 'city' should still be enforced")
	}
}

// --- a typeless (map form) any property is rejected in strict mode.

func TestStrict_TypelessPropertyErrors(t *testing.T) {
	// An `any` field carrying a jsonschema description reflects to a typeless
	// {"description":...} property — not a boolean schema — which must still be
	// rejected in strict mode instead of slipping through to a 400.
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"data": map[string]any{"description": "some data"}},
	}
	if _, err := EnsureStrictJSONSchema(schema); err == nil {
		t.Fatal("expected error for typeless (unconstrained) property")
	}
}

func TestNewFunctionTool_TypelessAnyFieldFailsAtConstruction(t *testing.T) {
	type badArgs struct {
		Data any `json:"data" jsonschema:"some data"`
	}
	tool := NewFunctionTool("bad", "",
		func(ctx context.Context, tc *ToolContext, a badArgs) (string, error) {
			return "ran", nil
		})
	if tool.constructionErr == nil {
		t.Fatal("expected constructionErr for a tagged any field (typeless schema)")
	}
	if _, err := tool.OnInvoke(context.Background(), &ToolContext{}, `{"data":1}`); err == nil {
		t.Fatal("a broken-schema tool must error on invoke")
	}
}

func TestStrict_TypedAndCombinatorPropertiesStillValid(t *testing.T) {
	// Guard against false positives: properties with a type, a $ref, or a
	// combinator must not be flagged as unconstrained.
	schema := map[string]any{
		"$defs": map[string]any{"Inner": map[string]any{"type": "string"}},
		"type":  "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"$ref": "#/$defs/Inner", "description": "d"},
			"c": map[string]any{"anyOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "number"},
			}},
			"d": map[string]any{"enum": []any{"x", "y"}},
		},
	}
	if _, err := EnsureStrictJSONSchema(schema); err != nil {
		t.Fatalf("well-typed properties should not error: %v", err)
	}
}

// --- the unconstrained-schema error no longer recommends json.RawMessage.

func TestStrict_UnconstrainedErrorMessageOmitsRawMessage(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"data": true}, // untagged any -> boolean schema
	}
	_, err := EnsureStrictJSONSchema(schema)
	if err == nil {
		t.Fatal("expected error for boolean-schema property")
	}
	if strings.Contains(err.Error(), "json.RawMessage") {
		t.Errorf("error must not recommend json.RawMessage (it reflects to a byte array): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unconstrained") {
		t.Errorf("error should describe the unconstrained schema: %q", err.Error())
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

func (p errModelProvider) GetModel(string) (Model, error) { return nil, p.err }

func TestFallbackProvider_AllFallbacksFailReturnsAggregatedError(t *testing.T) {
	primary := &stubProvider{model: &scriptedModel{}}
	fp := NewFallbackProvider(primary,
		errModelProvider{err: errors.New("f1 down")},
		errModelProvider{err: errors.New("f2 down")},
	)
	m, err := fp.GetModel("x")
	if err == nil {
		t.Fatal("expected aggregated error when every fallback fails to resolve")
	}
	if m != nil {
		t.Errorf("model should be nil on total fallback failure, got %T", m)
	}
	if !strings.Contains(err.Error(), "f1 down") || !strings.Contains(err.Error(), "f2 down") {
		t.Errorf("error should aggregate both fallback failures: %v", err)
	}
}

func TestFallbackProvider_PartialFallbackKeepsChain(t *testing.T) {
	primary := &stubProvider{model: &scriptedModel{}}
	good := &stubProvider{model: &scriptedModel{}}
	fp := NewFallbackProvider(primary, errModelProvider{err: errors.New("bad down")}, good)
	m, err := fp.GetModel("x")
	if err != nil {
		t.Fatalf("partial resolution should not error: %v", err)
	}
	if _, ok := m.(*FallbackModel); !ok {
		t.Errorf("expected a *FallbackModel chaining the working fallback, got %T", m)
	}
}

func TestFallbackProvider_NoFallbacksReturnsPrimary(t *testing.T) {
	primary := &stubProvider{model: &scriptedModel{}}
	m, err := NewFallbackProvider(primary).GetModel("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*FallbackModel); ok {
		t.Errorf("with no fallbacks configured, expected the bare primary model, got *FallbackModel")
	}
}

func TestFallbackProvider_PrimaryFailurePropagates(t *testing.T) {
	fp := NewFallbackProvider(errModelProvider{err: errBoom}, &stubProvider{model: &scriptedModel{}})
	if _, err := fp.GetModel("x"); !errors.Is(err, errBoom) {
		t.Fatalf("primary failure should propagate, got %v", err)
	}
}
