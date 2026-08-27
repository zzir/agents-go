package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// An e2b target validates its own fields and stores them canonically; the
// docker fields are not smuggled through a type that has none.
func TestNormalizeE2BTargetConfig(t *testing.T) {
	got, err := NormalizeTargetConfig("e2b", json.RawMessage(`{"api_url":"https://api.e2b.app","domain":"e2b.app","api_key":"k","unknown":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "unknown") {
		t.Errorf("canonical form kept an unknown key: %s", got)
	}
	for _, bad := range []string{
		`{"api_url":"api.e2b.app"}`,
		`{"data_plane_auth":"basic"}`,
	} {
		if _, err := NormalizeTargetConfig("e2b", json.RawMessage(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if _, err := NormalizeTargetConfig("quantum", nil); err == nil {
		t.Error("an unknown target type was accepted")
	}
}

// An e2b template must name a template that exists on the service: the
// workbench builds none.
func TestNormalizeE2BTemplateConfig(t *testing.T) {
	if _, err := NormalizeTemplateConfig("e2b", json.RawMessage(`{}`)); err == nil {
		t.Error("a template with no template_id was accepted")
	}
	if _, err := NormalizeTemplateConfig("e2b", json.RawMessage(`{"template_id":"base","timeout_seconds":-1}`)); err == nil {
		t.Error("a negative timeout was accepted")
	}
	got, err := NormalizeTemplateConfig("e2b", json.RawMessage(`{"template_id":"base","auto_pause":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"auto_pause":true`) {
		t.Errorf("canonical form = %s", got)
	}
}

// The destination a masked credential is bound to is per type: moving an e2b
// target's api_url is as much a move as changing a docker daemon's host.
func TestE2BTargetIdentity(t *testing.T) {
	if got := TargetDestinationField("e2b"); got != "api_url" {
		t.Errorf("destination field = %q, want api_url", got)
	}
	prev := &SandboxTarget{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://a","domain":"a"}`)}
	same := &SandboxTarget{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://a","domain":"a","api_key":"rotated"}`)}
	moved := &SandboxTarget{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://b","domain":"a"}`)}
	if TargetIdentityChanged(prev, same) {
		t.Error("rotating the key read as an identity change")
	}
	if !TargetIdentityChanged(prev, moved) {
		t.Error("moving the api_url did not read as an identity change")
	}
	// The domain is half the address too: sandboxes are reached through it.
	other := &SandboxTarget{Type: "e2b", Config: json.RawMessage(`{"api_url":"https://a","domain":"b"}`)}
	if !TargetIdentityChanged(prev, other) {
		t.Error("moving the domain did not read as an identity change")
	}
}

// An e2b api_key is sealed at rest like every other credential here — the one
// list covers both types, so a new backend's key cannot be missed.
func TestE2BAPIKeySealed(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	withTestBox(t)
	targets := NewSandboxTargetStore(db)
	tg := &SandboxTarget{Name: "cloud", Type: "e2b", Config: json.RawMessage(`{"api_url":"https://api.e2b.app","api_key":"e2b_live"}`)}
	if err := targets.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if raw := rawColumn(t, db, "SELECT config FROM sandbox_targets WHERE id = ?", tg.ID); strings.Contains(raw, "e2b_live") {
		t.Fatalf("api key at rest = %s", raw)
	}
	got, err := targets.Get(ctx, tg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Config), `"api_key":"e2b_live"`) {
		t.Fatalf("api key opened = %s", got.Config)
	}
}
