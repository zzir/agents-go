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

// The destination a masked credential is bound to is per type: moving an e2b
// sandbox's api_url is as much a move as changing a docker daemon's host.
func TestE2BSandboxIdentity(t *testing.T) {
	if got := SandboxDestinationField("e2b"); got != "api_url" {
		t.Errorf("destination field = %q, want api_url", got)
	}
	prev := &Sandbox{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://a","domain":"a"}`)}
	same := &Sandbox{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://a","domain":"a","api_key":"rotated"}`)}
	moved := &Sandbox{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://b","domain":"a"}`)}
	if SandboxIdentityChanged(prev, same) {
		t.Error("rotating the key read as an identity change")
	}
	if !SandboxIdentityChanged(prev, moved) {
		t.Error("moving the api_url did not read as an identity change")
	}
	// The domain is half the address too: sandboxes are reached through it.
	other := &Sandbox{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://a","domain":"b"}`)}
	if !SandboxIdentityChanged(prev, other) {
		t.Error("moving the domain did not read as an identity change")
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
