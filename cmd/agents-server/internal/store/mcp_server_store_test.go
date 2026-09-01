package store

import (
	"context"
	"testing"
)

// Update preserves the persisted OAuth grant by copying it forward inside the
// transaction — and hands it to prepare, which can therefore clear it. The
// handler clears it when the grant's identity (endpoint / auth mode / client
// id) moved; a token minted under one identity surviving into another is how
// a stale grant silently 403s every call.
func TestMcpServerUpdateGrantSurvivesUnlessCleared(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMcpServerStore(db)

	cfg := &McpServerConfig{Name: "srv", OwnerID: NewID(), Config: []byte(`{"endpoint":"https://a/mcp","auth_mode":"oauth"}`)}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveOAuthToken(ctx, cfg.ID, `{"access_token":"tok"}`); err != nil {
		t.Fatalf("save token: %v", err)
	}

	// A plain update (nil prepare) keeps the grant.
	upd := &McpServerConfig{Name: "srv2", Config: cfg.Config, Scope: cfg.Scope, OwnerID: cfg.OwnerID}
	if err := s.Update(ctx, cfg.ID, upd, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.Get(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OAuthToken != `{"access_token":"tok"}` {
		t.Fatalf("grant after plain update = %q, want preserved", got.OAuthToken)
	}

	// A prepare that clears the copied-forward token drops it in the same
	// transaction.
	upd = &McpServerConfig{Name: "srv3", Config: cfg.Config, Scope: cfg.Scope, OwnerID: cfg.OwnerID}
	if err := s.Update(ctx, cfg.ID, upd, func(prev *McpServerConfig) error {
		upd.OAuthToken = ""
		return nil
	}); err != nil {
		t.Fatalf("clearing update: %v", err)
	}
	if got, err = s.Get(ctx, cfg.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OAuthToken != "" {
		t.Fatalf("grant after clearing update = %q, want empty", got.OAuthToken)
	}
}
