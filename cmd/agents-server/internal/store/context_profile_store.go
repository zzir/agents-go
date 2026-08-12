package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
)

// ToolChars sizes what one tool definition costs in a request: its name,
// description and argument schema. The build measures the agent's own tools
// with it and the read path measures an MCP server's with the same rule, so
// the two halves of a profile are comparable.
func ToolChars(t *agents.Tool) int {
	n := len(t.Name) + len(t.Description)
	if t.ParamsJSONSchema != nil {
		if raw, err := json.Marshal(t.ParamsJSONSchema); err == nil {
			n += len(raw)
		}
	}
	return n
}

// ContextProfileStore persists one prompt/tool snapshot per session.
type ContextProfileStore struct {
	db *bun.DB
}

// NewContextProfileStore returns a store backed by db.
func NewContextProfileStore(db *bun.DB) *ContextProfileStore {
	return &ContextProfileStore{db: db}
}

// Save records the profile for sessionID, replacing whatever the previous run
// left: the panel reports what the session sends NOW, and a build that changed
// (a sandbox bound, an MCP server connected) must not be read as still current.
func (s *ContextProfileStore) Save(ctx context.Context, sessionID string, p PromptProfile) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding context profile for session %s: %w", sessionID, err)
	}
	row := &ContextProfile{SessionID: sessionID, Payload: string(payload)}
	if _, err := s.db.NewInsert().Model(row).
		On("CONFLICT (session_id) DO UPDATE").
		Set("payload = EXCLUDED.payload").
		Exec(ctx); err != nil {
		return fmt.Errorf("saving context profile for session %s: %w", sessionID, err)
	}
	return nil
}

// Get returns the session's profile, or nil when no run has built one yet.
func (s *ContextProfileStore) Get(ctx context.Context, sessionID string) (*PromptProfile, error) {
	row := new(ContextProfile)
	err := s.db.NewSelect().Model(row).Where("session_id = ?", sessionID).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("loading context profile for session %s: %w", sessionID, err)
	}
	var p PromptProfile
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
		return nil, fmt.Errorf("decoding context profile for session %s: %w", sessionID, err)
	}
	return &p, nil
}
