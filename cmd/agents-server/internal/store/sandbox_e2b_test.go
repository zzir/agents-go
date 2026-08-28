package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// An e2b sandbox validates its own fields and stores them canonically; the
// docker fields are not smuggled through a type that has none.
func TestNormalizeE2BConfig(t *testing.T) {
	got, err := NormalizeSandboxConfig("e2b", json.RawMessage(`{"api_url":"https://api.e2b.app","domain":"e2b.app","api_key":"k","template_id":"base","unknown":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "unknown") {
		t.Errorf("canonical form kept an unknown key: %s", got)
	}
	for _, bad := range []string{
		`{"api_url":"api.e2b.app","template_id":"base"}`,
		`{"data_plane_auth":"basic","template_id":"base"}`,
		`{}`, // an e2b sandbox must name a template that exists on the service
		`{"template_id":"base","timeout_seconds":-1}`,
	} {
		if _, err := NormalizeSandboxConfig("e2b", json.RawMessage(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if _, err := NormalizeSandboxConfig("quantum", nil); err == nil {
		t.Error("an unknown sandbox type was accepted")
	}
	if !strings.Contains(string(got), `"template_id":"base"`) {
		t.Errorf("canonical form = %s", got)
	}
}

// The destination a masked credential is bound to is per type, and BOTH the
// api_url and the domain are part of it for e2b: moving either must fire the
// mask-across-destination guard, or a stored key rides to a new host.
func TestE2BSandboxDestination(t *testing.T) {
	prev := json.RawMessage(`{"api_url":"https://a","domain":"a"}`)
	sameKey := json.RawMessage(`{"api_url":"https://a","domain":"a","api_key":"rotated"}`)
	movedURL := json.RawMessage(`{"api_url":"https://b","domain":"a"}`)
	movedDomain := json.RawMessage(`{"api_url":"https://a","domain":"b"}`)
	if SandboxDestinationChanged("e2b", prev, sameKey) {
		t.Error("rotating the key read as a destination change")
	}
	if !SandboxDestinationChanged("e2b", prev, movedURL) {
		t.Error("moving the api_url did not read as a destination change")
	}
	if !SandboxDestinationChanged("e2b", prev, movedDomain) {
		t.Error("moving the domain did not read as a destination change (the mask-guard leak)")
	}
	// The api_url/domain boundary must be self-delimiting: a "|" join renders
	// both of these as "https://a|b|c", collapsing a real move to "no change".
	splitA := json.RawMessage(`{"api_url":"https://a","domain":"b|c"}`)
	splitB := json.RawMessage(`{"api_url":"https://a|b","domain":"c"}`)
	if !SandboxDestinationChanged("e2b", splitA, splitB) {
		t.Error("a separator inside a destination field collided with the next field")
	}
}

// e2b freezes what a /connect resume cannot re-apply: the template and the
// lifecycle policy. timeout is NOT frozen — resume re-sends it — and rotating
// the key is not an identity change.
func TestE2BSandboxIdentity(t *testing.T) {
	base := `"api_url":"https://a","domain":"a","template_id":"base","timeout_seconds":300`
	prev := &Sandbox{Type: "e2b", Config: json.RawMessage(`{` + base + `}`)}
	cases := []struct {
		name   string
		config string
		frozen bool
	}{
		{"rotate key", `{` + base + `,"api_key":"rotated"}`, false},
		{"new timeout", `{"api_url":"https://a","domain":"a","template_id":"base","timeout_seconds":600}`, false},
		{"new template", `{"api_url":"https://a","domain":"a","template_id":"other","timeout_seconds":300}`, true},
		{"auto_pause", `{` + base + `,"auto_pause":true}`, true},
		{"allow_internet", `{` + base + `,"allow_internet":true}`, true},
		{"move domain", `{"api_url":"https://a","domain":"b","template_id":"base","timeout_seconds":300}`, true},
	}
	for _, tc := range cases {
		next := &Sandbox{Type: "e2b", Config: json.RawMessage(tc.config)}
		if got := SandboxIdentityChanged(prev, next); got != tc.frozen {
			t.Errorf("%s: identity changed = %v, want %v", tc.name, got, tc.frozen)
		}
	}
}

// An e2b api_key is sealed at rest like every other credential here — the one
// list covers both types, so a new backend's key cannot be missed.
func TestE2BAPIKeySealed(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	withTestBox(t)
	sandboxes := NewSandboxStore(db)
	sb := &Sandbox{Name: "cloud", Type: "e2b", Config: json.RawMessage(`{"api_url":"https://api.e2b.app","api_key":"e2b_live","template_id":"base"}`)}
	if err := sandboxes.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if raw := rawColumn(t, db, "SELECT config FROM sandboxes WHERE id = ?", sb.ID); strings.Contains(raw, "e2b_live") {
		t.Fatalf("api key at rest = %s", raw)
	}
	got, err := sandboxes.Get(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Config), `"api_key":"e2b_live"`) {
		t.Fatalf("api key opened = %s", got.Config)
	}
}
