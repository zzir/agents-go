package bridge

import (
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A guardrail definition must be validated at save time: unknown type/mode and
// an empty or uncompilable regex are rejected up front, not left to fail — or
// resolve to "not found" — when an agent later references the guardrail.
func TestValidateGuardrailDef(t *testing.T) {
	cases := []struct {
		name    string
		g       store.Guardrail
		wantSub string // "" = valid
	}{
		{"valid regex", store.Guardrail{Name: "n", Type: "input", Mode: "regex", Config: []byte(`{"pattern":"\\bx\\b"}`)}, ""},
		{"valid max_length", store.Guardrail{Name: "n", Type: "output", Mode: "max_length", Config: []byte(`{"max_length":100}`)}, ""},
		{"max_length default ok", store.Guardrail{Name: "n", Type: "input", Mode: "max_length"}, ""},
		{"unknown type", store.Guardrail{Name: "n", Type: "sideways", Mode: "regex", Config: []byte(`{"pattern":"x"}`)}, "type must be"},
		{"unknown mode", store.Guardrail{Name: "n", Type: "input", Mode: "banana"}, "mode must be"},
		{"empty regex", store.Guardrail{Name: "n", Type: "input", Mode: "regex"}, "non-empty config.pattern"},
		{"bad regex", store.Guardrail{Name: "n", Type: "input", Mode: "regex", Config: []byte(`{"pattern":"("}`)}, "invalid regex"},
		{"bad config json", store.Guardrail{Name: "n", Type: "input", Mode: "regex", Config: []byte(`{bad`)}, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGuardrailDef(&tc.g)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}
