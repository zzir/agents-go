package openai

import (
	"context"
	"fmt"
	"slices"
	"sync"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/conversations"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
)

// ConversationsSession is an agents.Session backed by the OpenAI Conversations
// API: history lives server-side under a conversation ID rather than in a local
// store. The conversation is created lazily on first use unless an existing ID
// is supplied via WithConversationID. It is the Go counterpart of the Python
// SDK's OpenAIConversationsSession.
//
// Item conversion reuses agents.UnmarshalInputItem, so the common item kinds
// (messages, function calls and their outputs) round-trip; exotic server-only
// item types may not.
type ConversationsSession struct {
	svc conversations.ConversationService

	mu sync.Mutex
	id string
}

var _ agents.Session = (*ConversationsSession)(nil)

// NewConversationsSession builds a ConversationsSession with its own OpenAI
// client. Pass openai-go request options (option.WithAPIKey, option.WithBaseURL,
// …) exactly as for NewProvider; with none, OPENAI_API_KEY is used.
func NewConversationsSession(opts ...option.RequestOption) *ConversationsSession {
	c := oai.NewClient(opts...)
	return &ConversationsSession{svc: c.Conversations}
}

// WithConversationID attaches the session to an existing server-side
// conversation instead of creating a new one on first use. It returns the
// session for chaining.
func (s *ConversationsSession) WithConversationID(id string) *ConversationsSession {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
	return s
}

// ConversationID returns the server-side conversation ID, creating the
// conversation if it does not exist yet.
func (s *ConversationsSession) ConversationID(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureID(ctx)
}

// ensureID returns the conversation ID, creating it on first use. The caller
// must hold s.mu.
func (s *ConversationsSession) ensureID(ctx context.Context) (string, error) {
	if s.id != "" {
		return s.id, nil
	}
	conv, err := s.svc.New(ctx, conversations.ConversationNewParams{
		Items: []responses.ResponseInputItemUnionParam{},
	})
	if err != nil {
		return "", fmt.Errorf("creating conversation: %w", err)
	}
	s.id = conv.ID
	return s.id, nil
}

// GetItems implements agents.Session. A limit <= 0 returns the whole
// conversation oldest-first; a positive limit returns the most recent `limit`
// items, also oldest-first.
func (s *ConversationsSession) GetItems(ctx context.Context, limit int) ([]agents.TResponseInputItem, error) {
	id, err := s.lockedEnsureID(ctx)
	if err != nil {
		return nil, err
	}

	params := conversations.ItemListParams{Order: conversations.ItemListParamsOrderAsc}
	if limit > 0 {
		// Fetch the newest `limit` items, then reverse to oldest-first below.
		params.Order = conversations.ItemListParamsOrderDesc
		params.Limit = oai.Int(int64(limit))
	}

	var items []agents.TResponseInputItem
	pager := s.svc.Items.ListAutoPaging(ctx, id, params)
	for pager.Next() {
		item, err := conversationItemToInput(pager.Current())
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if limit > 0 && len(items) >= limit {
			break
		}
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("listing conversation items: %w", err)
	}
	if limit > 0 {
		slices.Reverse(items)
	}
	return items, nil
}

// conversationItemsBatchLimit is the Conversations API's per-request cap on
// POST /conversations/{id}/items ("You may add up to 20 items at a time").
const conversationItemsBatchLimit = 20

// AddItems implements agents.Session. Items are appended in API-sized batches
// (conversationItemsBatchLimit per request), since the runner saves a whole
// run's items in one call and long runs easily exceed the per-request cap.
func (s *ConversationsSession) AddItems(ctx context.Context, in []agents.TResponseInputItem) error {
	if len(in) == 0 {
		return nil
	}
	id, err := s.lockedEnsureID(ctx)
	if err != nil {
		return err
	}
	for start := 0; start < len(in); start += conversationItemsBatchLimit {
		batch := in[start:min(start+conversationItemsBatchLimit, len(in))]
		if _, err := s.svc.Items.New(ctx, id, conversations.ItemNewParams{Items: batch}); err != nil {
			return fmt.Errorf("adding conversation items: %w", err)
		}
	}
	return nil
}

// PopItem implements agents.Session: it removes and returns the most recent
// item, or nil if the conversation is empty.
func (s *ConversationsSession) PopItem(ctx context.Context) (*agents.TResponseInputItem, error) {
	id, err := s.lockedEnsureID(ctx)
	if err != nil {
		return nil, err
	}
	pager := s.svc.Items.ListAutoPaging(ctx, id, conversations.ItemListParams{
		Order: conversations.ItemListParamsOrderDesc,
		Limit: oai.Int(1),
	})
	if !pager.Next() {
		if err := pager.Err(); err != nil {
			return nil, fmt.Errorf("listing conversation items: %w", err)
		}
		return nil, nil
	}
	raw := pager.Current()
	if raw.ID == "" {
		return nil, fmt.Errorf("conversation item has no ID; cannot pop")
	}
	item, err := conversationItemToInput(raw)
	if err != nil {
		return nil, err
	}
	if _, err := s.svc.Items.Delete(ctx, id, raw.ID); err != nil {
		return nil, fmt.Errorf("deleting conversation item: %w", err)
	}
	return &item, nil
}

// Clear implements agents.Session by deleting the server-side conversation. A
// fresh conversation is created on the next use.
func (s *ConversationsSession) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return nil
	}
	if _, err := s.svc.Delete(ctx, s.id); err != nil {
		return fmt.Errorf("deleting conversation: %w", err)
	}
	s.id = ""
	return nil
}

func (s *ConversationsSession) lockedEnsureID(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureID(ctx)
}

// conversationItemToInput converts a server conversation item back into an input
// item, reusing the runner's robust decoder.
func conversationItemToInput(item conversations.ConversationItemUnion) (agents.TResponseInputItem, error) {
	return agents.UnmarshalInputItem([]byte(item.RawJSON()))
}
