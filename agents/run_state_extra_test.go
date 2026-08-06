package agents_test

import (
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func stateJSON(version, extraField string) []byte {
	return []byte(`{"schema_version":"` + version + `","current_agent":"a","current_turn":1,` +
		`"original_input":[],"generated_items":[],"model_responses":[],` +
		`"interrupted_response":null,"interruptions":[]` + extraField + `}`)
}

// RunState.Extra is carried verbatim: the bytes a host stored come back
// byte-identical after decode and re-encode, whatever they hold — the SDK
// never parses them.
func TestRunStateExtraRoundTrips(t *testing.T) {
	registry := map[string]*agents.Agent{"a": {Name: "a"}}
	in := stateJSON(agents.RunStateSchemaVersion,
		`,"extra":{"plan:phase":{"unlocked":true},"host:note":"  raw,  spacing kept "}`)

	st, err := agents.RunStateFromJSON(in, registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(st.Extra["plan:phase"]); got != `{"unlocked":true}` {
		t.Errorf("Extra[plan:phase] = %s, want the stored object", got)
	}
	if got := string(st.Extra["host:note"]); got != `"  raw,  spacing kept "` {
		t.Errorf("Extra[host:note] = %s, want the raw string bytes", got)
	}

	out, err := st.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var reread struct {
		Extra map[string]json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(out, &reread); err != nil {
		t.Fatal(err)
	}
	if string(reread.Extra["plan:phase"]) != string(st.Extra["plan:phase"]) ||
		string(reread.Extra["host:note"]) != string(st.Extra["host:note"]) {
		t.Errorf("re-encoded extra differs from what was decoded: %s", out)
	}
}

// A state from before the field existed (schema 1.5) decodes with Extra nil —
// the additive-bump promise the version window is built on.
func TestRunStateExtraAbsentInOlderMinor(t *testing.T) {
	st, err := agents.RunStateFromJSON(stateJSON("1.5", ""), map[string]*agents.Agent{"a": {Name: "a"}})
	if err != nil {
		t.Fatalf("a 1.5 state must decode under this SDK: %v", err)
	}
	if st.Extra != nil {
		t.Errorf("Extra = %v, want nil for a pre-1.6 state", st.Extra)
	}
}

// The triage helper agrees with the decoder: what RunStateVersionSupported
// accepts, RunStateFromJSON decodes, and equality against the current version
// is NOT the rule.
func TestRunStateVersionSupportedMatchesDecoder(t *testing.T) {
	if !agents.RunStateVersionSupported("1.5") {
		t.Error("1.5 should be supported (window floor is 1.4)")
	}
	if !agents.RunStateVersionSupported(agents.RunStateSchemaVersion) {
		t.Error("the current version should be supported")
	}
	if agents.RunStateVersionSupported("1.1") {
		t.Error("1.1 is below the floor and must be refused")
	}
	if agents.RunStateVersionSupported("2.0") {
		t.Error("another major must be refused")
	}
}
