package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
)

// MessageStore reads stored messages for a session.
type MessageStore struct {
	db *bun.DB
}

// NewMessageStore returns a MessageStore backed by db.
func NewMessageStore(db *bun.DB) *MessageStore {
	return &MessageStore{db: db}
}

// GetMessages returns all messages for sessionID ordered oldest first.
func (s *MessageStore) GetMessages(ctx context.Context, sessionID string) ([]Message, error) {
	var msgs []Message
	if err := s.db.NewSelect().Model(&msgs).
		Where("session_id = ?", sessionID).
		OrderExpr("id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("getting messages: %w", err)
	}
	return msgs, nil
}

// SessionAdapter bridges the store's message table to the agents.Session
// interface so the SDK runner can load/save conversation history.
type SessionAdapter struct {
	db        *bun.DB
	sessionID string
}

// NewSessionAdapter returns a SessionAdapter bound to db and sessionID.
func NewSessionAdapter(db *bun.DB, sessionID string) *SessionAdapter {
	return &SessionAdapter{db: db, sessionID: sessionID}
}

// GetItems returns the session's stored input items oldest first; a positive
// limit returns only the most recent limit items (still in chronological order).
func (a *SessionAdapter) GetItems(ctx context.Context, limit int) ([]agents.TResponseInputItem, error) {
	q := a.db.NewSelect().Model((*Message)(nil)).
		Column("item").
		Where("session_id = ?", a.sessionID)

	if limit > 0 {
		q = q.OrderExpr("id DESC").Limit(limit)
	} else {
		q = q.OrderExpr("id ASC")
	}

	var msgs []Message
	if err := q.Scan(ctx, &msgs); err != nil {
		return nil, fmt.Errorf("session adapter get items: %w", err)
	}

	if limit > 0 {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}

	items := make([]agents.TResponseInputItem, 0, len(msgs))
	for _, m := range msgs {
		if m.Item == "" || m.Item == "{}" || m.Item == "null" {
			continue
		}
		item, err := agents.UnmarshalInputItem([]byte(m.Item))
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// AddItems appends items to the session, persisting each as a Message.
func (a *SessionAdapter) AddItems(ctx context.Context, items []agents.TResponseInputItem) error {
	if len(items) == 0 {
		return nil
	}
	msgs := make([]Message, 0, len(items))
	now := time.Now().UTC()
	for _, item := range items {
		raw, err := agents.MarshalInputItem(item)
		if err != nil {
			return fmt.Errorf("marshaling item: %w", err)
		}
		role, content := extractRoleContent(item, raw)
		msgs = append(msgs, Message{
			SessionID: a.sessionID,
			Role:      role,
			Content:   content,
			Item:      string(raw),
			CreatedAt: now,
		})
	}
	if _, err := a.db.NewInsert().Model(&msgs).Exec(ctx); err != nil {
		return fmt.Errorf("session adapter add items: %w", err)
	}
	return nil
}

func extractRoleContent(_ agents.TResponseInputItem, raw []byte) (role, content string) {
	var probe struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Name    string `json:"name"`
		Output  any    `json:"output"`
		Content any    `json:"content"`
	}
	_ = json.Unmarshal(raw, &probe)

	switch {
	case probe.Type == "function_call":
		args := extractJSONString(raw, "arguments")
		return "tool_call", probe.Name + "(" + args + ")"
	case probe.Type == "function_call_output":
		return "tool_output", extractJSONString(raw, "output")
	case probe.Role == "user":
		return "user", extractTextContent(raw)
	case probe.Role == "assistant" || probe.Type == "message":
		return "assistant", extractTextContent(raw)
	case probe.Role == "system" || probe.Role == "developer":
		return probe.Role, extractTextContent(raw)
	default:
		if probe.Type != "" {
			return probe.Type, ""
		}
		return "system", ""
	}
}

func extractTextContent(raw []byte) string {
	var simple struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &simple) == nil && simple.Content != "" {
		return simple.Content
	}
	var withParts struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &withParts) == nil {
		for _, p := range withParts.Content {
			if (p.Type == "input_text" || p.Type == "output_text") && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

func extractJSONString(raw []byte, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}

// PopItem removes and returns the most recently added item, or an error if the
// session is empty.
func (a *SessionAdapter) PopItem(ctx context.Context) (*agents.TResponseInputItem, error) {
	var msg Message
	err := a.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Model(&msg).
			Where("session_id = ?", a.sessionID).
			OrderExpr("id DESC").
			Limit(1).
			Scan(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*Message)(nil)).
			Where("id = ?", msg.ID).
			Exec(ctx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("session adapter pop item: %w", err)
	}
	item, err := agents.UnmarshalInputItem([]byte(msg.Item))
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// Clear deletes all of the session's stored items.
func (a *SessionAdapter) Clear(ctx context.Context) error {
	if _, err := a.db.NewDelete().Model((*Message)(nil)).
		Where("session_id = ?", a.sessionID).
		Exec(ctx); err != nil {
		return fmt.Errorf("session adapter clear: %w", err)
	}
	return nil
}

var _ agents.Session = (*SessionAdapter)(nil)
