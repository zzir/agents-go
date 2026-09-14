package store_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The fallback chain decodes as the array it is, and as the JSON string a row
// written before carried; an inline api_key is dropped and reported, a
// misspelled key is refused rather than running the entry on a default.
func TestFallbackModelsDecodeBothForms(t *testing.T) {
	var g store.ResilienceGroup
	if err := json.Unmarshal([]byte(`{"fallback_models":[{"provider_id":"p1","model":"m1"}]}`), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.FallbackModels) != 1 || g.FallbackModels[0].ProviderID != "p1" || g.FallbackModels[0].Model != "m1" {
		t.Fatalf("array form: %#v", g.FallbackModels)
	}
	if g.FallbackModels.InlineKeyAt() != -1 {
		t.Fatal("no entry carried a key")
	}

	legacy := `{"fallback_models":"[{\"model\":\"m\",\"provider_type\":\"anthropic\",\"base_url\":\"https://a.example\",\"api_key\":\"sk-old\"}]"}`
	if err := json.Unmarshal([]byte(legacy), &g); err != nil {
		t.Fatal(err)
	}
	e := g.FallbackModels[0]
	if e.ProviderType != "anthropic" || e.BaseURL != "https://a.example" || e.Model != "m" || e.ProviderID != "" {
		t.Fatalf("string form: %#v", e)
	}
	if g.FallbackModels.InlineKeyAt() != 0 {
		t.Fatal("the inline key must be reported")
	}
	out, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "sk-old") || strings.Contains(string(out), "api_key") {
		t.Fatalf("the key must not survive a re-encode: %s", out)
	}
	if err := json.Unmarshal([]byte(`{"fallback_models":""}`), &g); err != nil || g.FallbackModels != nil {
		t.Fatalf(`an empty string is unset: %v %#v`, err, g.FallbackModels)
	}

	for _, bad := range []string{
		`{"fallback_models":"{not json"}`,
		`{"fallback_models":[{"model":"m","provider":"anthropic"}]}`,
		`{"fallback_models":"[{\"model\":\"m\"}] {\"stray\":true}"}`,
	} {
		if err := json.Unmarshal([]byte(bad), &g); err == nil {
			t.Errorf("%s must not decode", bad)
		}
	}
}
