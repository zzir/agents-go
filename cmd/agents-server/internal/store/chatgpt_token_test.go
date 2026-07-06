package store

import (
	"context"
	"errors"
	"testing"
)

// SaveChatGPTToken against a non-existent agent must report ErrNotFound rather
// than silently succeeding — a no-op UPDATE would strand the OAuth token and
// make the callback look successful while the token is lost.
func TestSaveChatGPTTokenMissingAgentIsNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewAgentConfigStore(db)

	if err := s.SaveChatGPTToken(ctx, "does-not-exist", "{}"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("save to missing agent: err = %v, want ErrNotFound", err)
	}

	// A real agent saves and clears fine.
	ac := &AgentConfig{Name: "a", Model: "m"}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveChatGPTToken(ctx, ac.ID, "{}"); err != nil {
		t.Fatalf("save to existing agent: %v", err)
	}
	if err := s.ClearChatGPTToken(ctx, ac.ID); err != nil {
		t.Fatalf("clear existing agent: %v", err)
	}
	// Clearing a missing agent is also not-found.
	if err := s.ClearChatGPTToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear missing agent: err = %v, want ErrNotFound", err)
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

// Guardrails are unique per (type, name): the same name under different types is
// fine, but a duplicate (type, name) is rejected by the DB.
func TestGuardrailNameUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewGuardrailStore(db)
	if err := s.Create(ctx, &Guardrail{ID: NewID(), Name: "g", Type: "input", Mode: "max_length"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same name, different type: allowed.
	if err := s.Create(ctx, &Guardrail{ID: NewID(), Name: "g", Type: "output", Mode: "max_length"}); err != nil {
		t.Fatalf("same name different type should be allowed: %v", err)
	}
	// Duplicate (type, name): rejected, and classified for a 409.
	err := s.Create(ctx, &Guardrail{ID: NewID(), Name: "g", Type: "input", Mode: "regex"})
	if err == nil {
		t.Fatal("duplicate (type, name) must violate the unique index")
	}
	if _, ok := UniqueViolation(err); !ok {
		t.Errorf("UniqueViolation should classify the composite dup, got %v", err)
	}
}

// MCP server names are unique — the name is the tool-prefix namespace.
func TestMcpServerNameUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMcpServerStore(db)
	if err := s.Create(ctx, &McpServerConfig{ID: NewID(), Name: "fs", TransportType: "stdio"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.Create(ctx, &McpServerConfig{ID: NewID(), Name: "fs", TransportType: "stdio"})
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
	if err := s.SaveOAuthToken(ctx, "ghost", `{"access_token":"x"}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveOAuthToken(missing) err = %v, want ErrNotFound", err)
	}
	// An existing server persists fine.
	cfg := &McpServerConfig{ID: NewID(), Name: "s", TransportType: "stdio"}
	if err := s.Create(ctx, cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveOAuthToken(ctx, cfg.ID, `{"access_token":"x"}`); err != nil {
		t.Fatalf("SaveOAuthToken(existing) err = %v, want nil", err)
	}
}
