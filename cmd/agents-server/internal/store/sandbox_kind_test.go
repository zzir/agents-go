package store

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// A semantic question about a type outside the descriptor map fails loudly:
// NormalizeSandboxConfig could never have stored it, so a quiet per-type
// default would be a wrong answer, not a safe one.
func TestSandboxKindUnknownTypePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("SandboxSupportsFor on an unknown type did not panic")
		}
	}()
	SandboxSupportsFor("quantum")
}

// Published ports are declared only on types that say so; a type without the
// flag — unknown included — refuses a declared list instead of storing a
// phantom.
func TestCheckPortsSupportedByKind(t *testing.T) {
	ports := `[8080]`
	if err := checkPortsSupported("docker", ports); err != nil {
		t.Errorf("docker refused declared ports: %v", err)
	}
	for _, typ := range []string{"e2b", "quantum"} {
		if err := checkPortsSupported(typ, ports); !errors.Is(err, ErrPortsUnsupported) {
			t.Errorf("%s: err = %v, want ErrPortsUnsupported", typ, err)
		}
		if err := checkPortsSupported(typ, ""); err != nil {
			t.Errorf("%s with no ports: %v", typ, err)
		}
	}
}

// The capability row the API exposes, per type — pinned: the frontend keys
// off these exact values.
func TestSandboxSupportsPerType(t *testing.T) {
	if got, want := SandboxSupportsFor("docker"), (SandboxSupports{Rebuild: true}); got != want {
		t.Errorf("docker = %+v, want %+v", got, want)
	}
	if got, want := SandboxSupportsFor("e2b"), (SandboxSupports{AnyPort: true, PublicPorts: true}); got != want {
		t.Errorf("e2b = %+v, want %+v", got, want)
	}
	if want := []string{"docker", "e2b"}; !slices.Equal(SandboxTypes, want) {
		t.Errorf("SandboxTypes = %v, want %v", SandboxTypes, want)
	}
}

// The identity-conflict 409 keeps its per-type wording.
func TestSandboxFrozenFieldsPerType(t *testing.T) {
	if got := SandboxFrozenFields("docker"); got != "its type and machine are frozen — the image, the limits, the credential and the name stay editable" {
		t.Errorf("docker = %q", got)
	}
	if got := SandboxFrozenFields("e2b"); got != "its type, service address, template and lifecycle (auto-pause, internet) are frozen — the api key, timeout, read limit and name stay editable" {
		t.Errorf("e2b = %q", got)
	}
}

// The storage hint's per-sandbox half keeps its exact strings per type.
func TestSandboxStorageWherePerType(t *testing.T) {
	cases := []struct{ typ, config, want string }{
		{"docker", `{"image":"i"}`, "the local daemon"},
		{"docker", `{"host":"ssh://u@h","image":"i"}`, "ssh://u@h"},
		{"e2b", `{"template_id":"base"}`, "sandbox on https://api.e2b.app"},
		{"e2b", `{"api_url":"https://svc","template_id":"base"}`, "sandbox on https://svc"},
	}
	for _, tc := range cases {
		sb := &Sandbox{Type: tc.typ, Config: json.RawMessage(tc.config)}
		if got := SandboxStorageWhere(sb); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.typ, tc.config, got, tc.want)
		}
	}
}
