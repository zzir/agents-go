package modelkit

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
)

// passthroughSettings are the ModelSettings fields deliberately outside the
// feature vocabulary: they are verbatim provider escape hatches, so their
// contents mean nothing to an adapter and there is no capability to declare
// unsupported. A caller who sets them accepts responsibility for the target
// provider understanding them.
var passthroughSettings = map[string]bool{
	"ExtraHeaders": true,
	"ExtraQuery":   true,
	"ExtraBody":    true,
}

// TestFeatureSetCoversEverySetting is the completeness guard for the feature
// vocabulary. featureSet is hand-written, and a setting missing from it hits
// exactly the failure fail-loud exists to prevent: a backend with no
// equivalent can neither send that setting nor Reject it, so it is dropped in
// silence. The walk is by reflection so the next ModelSettings field added
// cannot slip past.
func TestFeatureSetCoversEverySetting(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[agents.ModelSettings]()
	for name := range passthroughSettings {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("passthroughSettings exempts %s, which is no longer a ModelSettings field", name)
		}
	}

	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() || passthroughSettings[field.Name] {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			sample, ok := nonZeroSample(field.Type)
			if !ok {
				t.Fatalf("no non-zero sample for %s (type %s): teach nonZeroSample about that type",
					field.Name, field.Type)
			}
			settings := &agents.ModelSettings{}
			reflect.ValueOf(settings).Elem().Field(i).Set(sample)

			req := agents.ModelRequest{Settings: settings}
			covered := false
			for _, isSet := range featureSet {
				if isSet(req) {
					covered = true
					break
				}
			}
			if !covered {
				t.Errorf("no featureSet predicate reports ModelSettings.%s as set: a backend that cannot "+
					"serve it would drop it silently\nadd a Feature constant plus its featureSet entry in "+
					"features.go, or exempt the field in passthroughSettings here with a reason",
					field.Name)
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
// on. A zero sample leaves every featureSet predicate reporting false for an
// honest reason, which reads as a missing Feature entry instead of a probe
// that cannot represent the field — an unfillable type has to be reported as
// one, at any nesting depth.
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

func TestRejectIgnoresUnusedFeatures(t *testing.T) {
	req := agents.ModelRequest{Settings: &agents.ModelSettings{Temperature: new(0.2)}}
	if err := Reject("prov", req, FeatureServiceTier, FeatureVerbosity, FeaturePreviousResponseID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRejectNamesTheFeature(t *testing.T) {
	req := agents.ModelRequest{Settings: &agents.ModelSettings{ServiceTier: agents.ServiceTierFlex}}
	err := Reject("prov", req, FeatureServiceTier)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := errors.AsType[*agents.UserError](err); !ok {
		t.Fatalf("expected *agents.UserError, got %T", err)
	}
	if !strings.Contains(err.Error(), "service_tier") || !strings.Contains(err.Error(), "prov") {
		t.Fatalf("error should name provider and feature: %v", err)
	}
}

func TestRejectRequestLevelFeatures(t *testing.T) {
	req := agents.ModelRequest{PreviousResponseID: "resp_1"}
	if err := Reject("prov", req, FeaturePreviousResponseID); err == nil {
		t.Fatal("expected error for previous_response_id")
	}
	req = agents.ModelRequest{ConversationID: "conv_1"}
	if err := Reject("prov", req, FeatureConversationID); err == nil {
		t.Fatal("expected error for conversation_id")
	}
	req = agents.ModelRequest{Prompt: &agents.Prompt{ID: "p"}}
	if err := Reject("prov", req, FeaturePrompt); err == nil {
		t.Fatal("expected error for prompt")
	}
}

func TestRejectReasoningSummaryIsSeparateFromReasoning(t *testing.T) {
	req := agents.ModelRequest{Settings: &agents.ModelSettings{
		Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortHigh},
	}}
	if err := Reject("prov", req, FeatureReasoningSummary); err != nil {
		t.Fatalf("effort alone must not trip reasoning.summary: %v", err)
	}
	req.Settings.Reasoning.Summary = agents.ReasoningSummaryAuto
	if err := Reject("prov", req, FeatureReasoningSummary); err == nil {
		t.Fatal("expected error once summary is set")
	}
}

func TestRejectUnknownFeatureFailsLoud(t *testing.T) {
	if err := Reject("prov", agents.ModelRequest{}, Feature("bogus")); err == nil {
		t.Fatal("expected error for unknown feature name")
	}
}
