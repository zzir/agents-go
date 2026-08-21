package agents

import (
	"reflect"
	"testing"
	"time"
)

// TestResolveCoversEverySetting is the completeness guard for Resolve. Every
// field is overlaid by its own hand-written if-block, and a field whose block
// is missing fails silently: the run-level override simply never applies. The
// walk is by reflection so the next field added cannot skip Resolve unnoticed.
func TestResolveCoversEverySetting(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[ModelSettings]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			sample, ok := nonZeroSample(field.Type)
			if !ok {
				t.Fatalf("no non-zero sample for %s (type %s): teach nonZeroSample about that type",
					field.Name, field.Type)
			}
			want := sample.Interface()

			override := &ModelSettings{}
			reflect.ValueOf(override).Elem().Field(i).Set(sample)
			got := reflect.ValueOf((&ModelSettings{}).Resolve(override)).Elem().Field(i).Interface()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Resolve dropped the override for %s: got %#v, want %#v\n"+
					"add an `if override.%s is set { out.%s = override.%s }` block to Resolve",
					field.Name, got, want, field.Name, field.Name, field.Name)
			}

			base := &ModelSettings{}
			reflect.ValueOf(base).Elem().Field(i).Set(sample)
			kept := reflect.ValueOf(base.Resolve(&ModelSettings{})).Elem().Field(i).Interface()
			if !reflect.DeepEqual(kept, want) {
				t.Errorf("Resolve cleared the base %s although the override left it unset: got %#v",
					field.Name, kept)
			}
		})
	}
}

// nonZeroSample builds a fully populated value of type t, to stand for "this
// setting was configured". It reports false for a type it cannot fill, so a
// future field of an unfamiliar shape fails the guard above instead of
// probing it with a zero value that proves nothing.
func nonZeroSample(t reflect.Type) (reflect.Value, bool) {
	switch t.Kind() {
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(t), true
	case reflect.String:
		return reflect.ValueOf("sample").Convert(t), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflect.ValueOf(int64(1)).Convert(t), true
	case reflect.Float32, reflect.Float64:
		return reflect.ValueOf(0.5).Convert(t), true
	case reflect.Interface:
		// Only the empty interface: a string satisfies it and nothing else.
		if t.NumMethod() != 0 {
			return reflect.Value{}, false
		}
		return reflect.ValueOf("sample"), true
	case reflect.Pointer:
		elem, ok := nonZeroSample(t.Elem())
		if !ok {
			return reflect.Value{}, false
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(elem)
		return p, true
	case reflect.Slice:
		elem, ok := nonZeroSample(t.Elem())
		if !ok {
			return reflect.Value{}, false
		}
		return reflect.Append(reflect.MakeSlice(t, 0, 1), elem), true
	case reflect.Map:
		key, ok := nonZeroSample(t.Key())
		if !ok {
			return reflect.Value{}, false
		}
		val, ok := nonZeroSample(t.Elem())
		if !ok {
			return reflect.Value{}, false
		}
		m := reflect.MakeMap(t)
		m.SetMapIndex(key, val)
		return m, true
	case reflect.Struct:
		s := reflect.New(t).Elem()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fv, ok := nonZeroSample(f.Type)
			if !ok {
				return reflect.Value{}, false
			}
			s.Field(i).Set(fv)
		}
		// Nothing reachable to fill — time.Time and every other struct that
		// keeps its state unexported. The sample is still the zero value, and
		// a zero probe cannot tell "the code handles this field" apart from
		// "the code ignores it".
		if s.IsZero() {
			return reflect.Value{}, false
		}
		return s, true
	default:
		return reflect.Value{}, false
	}
}

// TestNonZeroSampleRefusesZeroProbes pins the contract the guard above rests
// on. The guard compares a field against its sample, so a zero sample passes
// whatever Resolve does with that field — an unfillable type has to be
// reported as one, at any nesting depth.
func TestNonZeroSampleRefusesZeroProbes(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[time.Time](),             // a struct, but all of its state is unexported
		reflect.TypeOf(struct{ At time.Time }{}), // and one level down
	} {
		if sample, ok := nonZeroSample(typ); ok && sample.IsZero() {
			t.Errorf("nonZeroSample(%s) offered the zero value as a usable sample", typ)
		}
	}
}

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
	got := base.Resolve(&ModelSettings{Temperature: new(0.5)})
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
	cm := []ContextManagement{{Type: "compaction", CompactThreshold: new(int64(200000))}}
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
