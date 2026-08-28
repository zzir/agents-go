package store

import (
	"strings"
	"testing"
)

// The canonical payload is sorted by key, whatever order it arrived in — the
// order storage, the container fingerprint and EnvContentEqual all share.
func TestNormalizeProjectEnvCanonical(t *testing.T) {
	got, err := NormalizeProjectEnv([]EnvVar{{Key: "B", Value: "2"}, {Key: "A", Value: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"key":"A","value":"1"},{"key":"B","value":"2"}]`
	if got != want {
		t.Errorf("canonical = %s, want %s", got, want)
	}
	// An empty environment is no environment: the empty string, so a project
	// without one stays off the sandbox fingerprint (spec §2.7n).
	if got, err := NormalizeProjectEnv(nil); err != nil || got != "" {
		t.Errorf("empty environment = %q, %v; want \"\"", got, err)
	}
	if got, err := NormalizeProjectEnv([]EnvVar{}); err != nil || got != "" {
		t.Errorf("empty slice = %q, %v; want \"\"", got, err)
	}
}

func TestNormalizeProjectEnvRejects(t *testing.T) {
	tests := map[string]struct {
		vars []EnvVar
		want string
	}{
		"empty name":     {[]EnvVar{{Key: "", Value: "v"}}, "must match"},
		"leading digit":  {[]EnvVar{{Key: "1PATH", Value: "v"}}, "must match"},
		"equals in name": {[]EnvVar{{Key: "A=B", Value: "v"}}, "must match"},
		"dash in name":   {[]EnvVar{{Key: "A-B", Value: "v"}}, "must match"},
		"duplicate":      {[]EnvVar{{Key: "A", Value: "1"}, {Key: "A", Value: "2"}}, "set twice"},
		"value too big":  {[]EnvVar{{Key: "A", Value: strings.Repeat("x", MaxEnvValueBytes+1)}}, "over the"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeProjectEnv(tc.vars); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}

	many := make([]EnvVar, MaxEnvVars+1)
	for i := range many {
		many[i] = EnvVar{Key: "K" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Value: "v"}
	}
	if _, err := NormalizeProjectEnv(many); err == nil {
		t.Error("too many variables were accepted")
	}

	// Each value under the per-value cap, the total over the whole-environment one.
	big := make([]EnvVar, 8)
	for i := range big {
		big[i] = EnvVar{Key: "K" + string(rune('a'+i)), Value: strings.Repeat("x", MaxEnvValueBytes)}
	}
	if _, err := NormalizeProjectEnv(big); err == nil || !strings.Contains(err.Error(), "environment is") {
		t.Errorf("total-size err = %v, want the whole-environment refusal", err)
	}
}

// The predicate behind the runtime-generation bump: what the container gets,
// nothing else.
func TestEnvContentEqual(t *testing.T) {
	shown, err := NormalizeProjectEnv([]EnvVar{{Key: "TOKEN", Value: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	same, err := NormalizeProjectEnv([]EnvVar{{Key: "TOKEN", Value: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	if !EnvContentEqual(shown, same) {
		t.Error("identical environments compared unequal")
	}
	changed, _ := NormalizeProjectEnv([]EnvVar{{Key: "TOKEN", Value: "other"}})
	if EnvContentEqual(shown, changed) {
		t.Error("a changed value compared as equal")
	}
	added, _ := NormalizeProjectEnv([]EnvVar{{Key: "TOKEN", Value: "t"}, {Key: "MORE", Value: "m"}})
	if EnvContentEqual(shown, added) {
		t.Error("an added variable compared as equal")
	}
	if !EnvContentEqual("", "") {
		t.Error("two empty environments compared unequal")
	}
	// Undecodable compares unequal — the safe side.
	if EnvContentEqual("{not json", "{not json") {
		t.Error("undecodable payloads compared equal")
	}
}

func TestEnvMap(t *testing.T) {
	raw, err := NormalizeProjectEnv([]EnvVar{{Key: "A", Value: "1"}, {Key: "B", Value: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := EnvMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["A"] != "1" || got["B"] != "2" {
		t.Errorf("EnvMap = %v, want A=1 B=2", got)
	}
	if got, err := EnvMap(""); err != nil || got != nil {
		t.Errorf("EnvMap(\"\") = %v, %v; want nil, nil", got, err)
	}
	// Refused rather than silently starting a container without them.
	if _, err := EnvMap("{not json"); err == nil {
		t.Error("an undecodable environment produced a map instead of an error")
	}
}

// A port list is stored canonically — deduplicated and ordered — so the
// container fingerprint and the change comparison answer in one order.
func TestNormalizeProjectPorts(t *testing.T) {
	got, err := NormalizeProjectPorts([]int{5173, 3000, 5173})
	if err != nil {
		t.Fatal(err)
	}
	if got != "[3000,5173]" {
		t.Fatalf("canonical = %s, want [3000,5173]", got)
	}
	if empty, err := NormalizeProjectPorts(nil); err != nil || empty != "" {
		t.Fatalf("no ports = %q/%v, want \"\"/nil", empty, err)
	}
	for _, bad := range [][]int{{0}, {65536}, {-1}} {
		if _, err := NormalizeProjectPorts(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if _, err := NormalizeProjectPorts(make([]int, MaxProjectPorts+1)); err == nil {
		t.Error("an oversized list was accepted")
	}
	// Order of writing must not read as a change.
	a, _ := NormalizeProjectPorts([]int{3000, 5173})
	b, _ := NormalizeProjectPorts([]int{5173, 3000})
	if !PortsContentEqual(a, b) {
		t.Errorf("%s and %s compared unequal", a, b)
	}
	if PortsContentEqual(a, "[3000]") {
		t.Error("a shorter list compared equal")
	}
}
