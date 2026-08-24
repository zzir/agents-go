package store

import (
	"context"
	"errors"
	"testing"
)

// SaveChatGPTToken against a non-existent provider must report ErrNotFound
// rather than silently succeeding — a no-op UPDATE would strand the OAuth token
// and make the callback look successful while the token is lost.
func TestSaveChatGPTTokenMissingProviderIsNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewProviderStore(db)

	if err := s.SaveChatGPTToken(ctx, NewID(), "{}"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("save to missing provider: err = %v, want ErrNotFound", err)
	}

	// A real provider saves and clears fine.
	pv := &Provider{Name: "a"}
	if err := s.Create(ctx, pv); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveChatGPTToken(ctx, pv.ID, "{}"); err != nil {
		t.Fatalf("save to existing provider: %v", err)
	}
	if err := s.ClearChatGPTToken(ctx, pv.ID); err != nil {
		t.Fatalf("clear existing provider: %v", err)
	}
	// Clearing a missing provider is also not-found.
	if err := s.ClearChatGPTToken(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear missing provider: err = %v, want ErrNotFound", err)
	}
}

// A provider's name is how every config UI names it, so the DB enforces
// uniqueness and the violation classifies for a 409.
func TestProviderNameUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewProviderStore(db)
	if err := s.Create(ctx, &Provider{Name: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, ok := UniqueViolation(s.Create(ctx, &Provider{Name: "dup"})); !ok {
		t.Error("duplicate provider name must violate the unique index")
	}
}

// The DB enforces agent-name uniqueness (HITL resume serializes the current
// agent by name, so a duplicate would bind to the wrong config), and the
// violation is a UNIQUE-constraint error that UniqueViolation classifies for a
// 409.
func TestAgentConfigNameUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewAgentConfigStore(db)
	if err := s.Create(ctx, &AgentConfig{Name: "dup", Model: "m"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := s.Create(ctx, &AgentConfig{Name: "dup", Model: "m2"})
	if err == nil {
		t.Fatal("second create with duplicate name must violate the unique index")
	}
	if cols, ok := UniqueViolation(err); !ok || cols != "name" {
		t.Errorf("UniqueViolation = %q,%v want \"name\",true", cols, ok)
	}
}

// A guardrail's name is its identity: an agent config references it by name and
// nothing else, so two definitions sharing one are ambiguous. Stages no longer
// disambiguate them — one definition carries all the stages it inspects.
func TestGuardrailNameUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewGuardrailStore(db)
	if err := s.Create(ctx, &Guardrail{ID: NewID(), Name: "g", Stages: []string{"input"}, Mode: "max_length"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Differing stages do not make it a different guardrail.
	err := s.Create(ctx, &Guardrail{ID: NewID(), Name: "g", Stages: []string{"output"}, Mode: "max_length"})
	if err == nil {
		t.Fatal("duplicate name must violate the unique index")
	}
	if _, ok := UniqueViolation(err); !ok {
		t.Errorf("UniqueViolation should classify the dup, got %v", err)
	}
}

// MCP server names are unique — the name is the tool-prefix namespace.
func TestMcpServerNameUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMcpServerStore(db)
	if err := s.Create(ctx, &McpServerConfig{ID: NewID(), Name: "fs"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.Create(ctx, &McpServerConfig{ID: NewID(), Name: "fs"})
	if err == nil {
		t.Fatal("duplicate mcp server name must violate the unique index")
	}
	if cols, ok := UniqueViolation(err); !ok || cols != "name" {
		t.Errorf("UniqueViolation = %q,%v want \"name\",true", cols, ok)
	}
}

// SaveOAuthToken on a deleted/nonexistent server reports ErrNotFound rather than
// silently succeeding, so the OAuth flow can't mistake a lost write for success.
func TestSaveOAuthTokenNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMcpServerStore(db)
	if err := s.SaveOAuthToken(ctx, NewID(), `{"access_token":"x"}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveOAuthToken(missing) err = %v, want ErrNotFound", err)
	}
	// An existing server persists fine.
	cfg := &McpServerConfig{ID: NewID(), Name: "s"}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveOAuthToken(ctx, cfg.ID, `{"access_token":"x"}`); err != nil {
		t.Fatalf("SaveOAuthToken(existing) err = %v, want nil", err)
	}
}
