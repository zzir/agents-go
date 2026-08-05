package openai

import (
	"context"
	"encoding/json"
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
// API: history lives server-side under a conversation ID rather than in a
// local store. The conversation is created lazily on first use unless an
// existing ID is supplied via SetConversationID.
//
// Item conversion reuses agents.UnmarshalInputItem, so the common item kinds
// (messages, function calls and their outputs) round-trip; exotic server-only
// item types may not.
type ConversationsSession struct {
	svc conversations.ConversationService

	mu sync.Mutex
	id string
}

var _ agents.SessionStorage = (*ConversationsSession)(nil)

// NewConversationsSession builds a ConversationsSession with its own OpenAI
// client. Pass openai-go request options (option.WithAPIKey, option.WithBaseURL,
// …) exactly as for NewProvider; with none, OPENAI_API_KEY is used.
func NewConversationsSession(opts ...option.RequestOption) *ConversationsSession {
	c := oai.NewClient(opts...)
	return &ConversationsSession{svc: c.Conversations}
}

// SetConversationID attaches the session to an existing server-side
// conversation instead of creating a new one on first use.
func (s *ConversationsSession) SetConversationID(id string) {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
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

// Entries implements agents.SessionStorage, oldest-first. A negative cursor
// limit fetches the most recent -Limit entries.
//
// Every entry is an item entry: the server holds Responses items and nothing
// else, so nothing a run recorded outside the conversation itself comes back.
func (s *ConversationsSession) Entries(ctx context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	fetch := 0
	if cur.Limit < 0 {
		fetch = -cur.Limit
	}
	entries, err := s.listEntries(ctx, fetch)
	if err != nil {
		return nil, err
	}
	// Position in what the server returned, which is the only number available
	// here: the conversation lives on the server, so there is no local store to
	// have allocated one. It therefore does NOT satisfy the "never moves"
	// guarantee an entry from a local store does — if the server ever stops
	// returning an item, everything after it shifts. Reading the most recent N
	// (a negative limit) is unaffected; resuming from AfterSeq is best-effort.
	for i := range entries {
		entries[i].Seq = int64(i + 1)
	}
	page := agents.Cursor{AfterSeq: cur.AfterSeq}
	if cur.Limit > 0 {
		// A positive limit is part of the SessionStorage contract too; it was
		// once silently dropped, and a pager asking for 50 got the whole
		// conversation.
		page.Limit = cur.Limit
	}
	return agents.PageEntries(entries, page), nil
}

// Metadata implements agents.SessionStorage. The server owns the conversation,
// so only its id is known here.
func (s *ConversationsSession) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	id, err := s.ConversationID(ctx)
	if err != nil {
		return agents.SessionMetadata{}, err
	}
	return agents.SessionMetadata{ID: id}, nil
}

// Entry implements agents.SessionStorage by scanning the conversation; the API
// has no per-item fetch.
func (s *ConversationsSession) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
	if id == "" {
		return nil, nil
	}
	entries, err := s.Entries(ctx, agents.Cursor{})
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// listEntries reads the conversation as entries, carrying each server item's
// ID onto the entry it becomes. The server is the id authority here — an entry
// whose id were left blank could never be found again through Entry, and
// "a caller that needs an entry reads its id back" (spec §2.5e2) would be
// unsatisfiable against this store.
func (s *ConversationsSession) listEntries(ctx context.Context, limit int) ([]agents.SessionEntry, error) {
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

	var entries []agents.SessionEntry
	pager := s.svc.Items.ListAutoPaging(ctx, id, params)
	for pager.Next() {
		raw := pager.Current()
		item, err := conversationItemToInput(raw)
		if err != nil {
			return nil, err
		}
		entry, err := agents.NewItemEntry(item, agents.Source{})
		if err != nil {
			return nil, err
		}
		entry.ID = raw.ID
		entries = append(entries, entry)
		if limit > 0 && len(entries) >= limit {
			break
		}
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("listing conversation items: %w", err)
	}
	if limit > 0 {
		slices.Reverse(entries)
	}
	return entries, nil
}

// conversationItemsBatchLimit is the Conversations API's per-request cap on
// POST /conversations/{id}/items ("You may add up to 20 items at a time").
const conversationItemsBatchLimit = 20

// Append implements agents.SessionStorage.
//
// **Only item entries are stored.** A server-managed conversation holds
// Responses items; there is nowhere on the server for an annotation, a terminal
// record or a custom entry, so those are dropped rather than failing the write.
// Losing a UI annotation degrades a timeline; failing the run because one could
// not be stored server-side is worse. Use a local Session when everything a run
// records must survive.
func (s *ConversationsSession) Append(ctx context.Context, entries ...agents.SessionEntry) error {
	items := make([]agents.TResponseInputItem, 0, len(entries))
	for _, e := range entries {
		if e.Kind != agents.EntryKindItem {
			continue
		}
		item, err := e.InputItem()
		if err != nil {
			return err
		}
		items = append(items, item)
	}
	return s.addItems(ctx, items)
}

// addItems appends items in API-sized batches
// (conversationItemsBatchLimit per request), since the runner saves a whole
// run's items in one call and long runs easily exceed the per-request cap.
//
// Each item is sanitized for the Conversations API before persistence
// (sanitizeConversationItem): provider-only fields are dropped, stale top-level
// ids are stripped except where the create-item schema requires them, and
// reasoning items lacking both an id and encrypted content are omitted entirely.
func (s *ConversationsSession) addItems(ctx context.Context, in []agents.TResponseInputItem) error {
	if len(in) == 0 {
		return nil
	}
	// Serialized with PopEntry for the same reason its list+delete is: an
	// append interleaving with a pop makes the pop remove an item that is no
	// longer the most recent.
	s.mu.Lock()
	defer s.mu.Unlock()
	sanitized := make([]agents.TResponseInputItem, 0, len(in))
	for _, item := range in {
		clean, keep, err := sanitizeConversationItem(item)
		if err != nil {
			return fmt.Errorf("sanitizing conversation item: %w", err)
		}
		if !keep {
			continue
		}
		sanitized = append(sanitized, clean)
	}
	if len(sanitized) == 0 {
		return nil
	}
	id, err := s.ensureID(ctx)
	if err != nil {
		return err
	}
	// The Conversations API commits each batch independently and offers no way to
	// roll one back, so a failure after the first batch leaves the server-side
	// conversation holding a partial turn. We cannot undo it here; instead the
	// error reports how much was already written so the caller can reconcile
	// (e.g. reset the session) rather than silently proceeding on half-state.
	written := 0
	for start := 0; start < len(sanitized); start += conversationItemsBatchLimit {
		end := min(start+conversationItemsBatchLimit, len(sanitized))
		batch := sanitized[start:end]
		if _, err := s.svc.Items.New(ctx, id, conversations.ItemNewParams{Items: batch}); err != nil {
			if written > 0 {
				return fmt.Errorf("adding conversation items: failed after writing %d of %d item(s) in prior batches; conversation may be left in a partially written state: %w", written, len(sanitized), err)
			}
			return fmt.Errorf("adding conversation items: %w", err)
		}
		written = end
	}
	return nil
}

// conversationItemTypesWithRequiredID lists the Responses input item types whose
// top-level id the Conversations create-item schema requires; every other type's
// id is stripped before persistence.
var conversationItemTypesWithRequiredID = map[string]bool{
	"file_search_call":        true,
	"web_search_call":         true,
	"computer_call":           true,
	"code_interpreter_call":   true,
	"image_generation_call":   true,
	"local_shell_call":        true,
	"local_shell_call_output": true,
	"mcp_list_tools":          true,
	"mcp_approval_request":    true,
	"mcp_call":                true,
	"item_reference":          true,
}

// sanitizeConversationItem strips provider-specific fields from an item before
// it is persisted through the Conversations API.
//
// It returns the sanitized item and whether it should be persisted at all: a
// reasoning item lacking both a server id and encrypted content is unpersistable
// (keep == false). Non-object items pass through unchanged.
func sanitizeConversationItem(item agents.TResponseInputItem) (agents.TResponseInputItem, bool, error) {
	raw, err := json.Marshal(item)
	if err != nil {
		return item, false, err
	}
	var m map[string]any
	// raw is a freshly marshaled item, so only non-object JSON can fail to decode
	// into a map; either way m stays nil and the item passes through untouched.
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		return item, true, nil
	}

	typ, _ := m["type"].(string)
	if typ == "reasoning" {
		// Reasoning items keep their id (required to remain persistable), but are
		// dropped when they carry neither an id nor encrypted content.
		if !isNonEmptyString(m["id"]) && !isNonEmptyString(m["encrypted_content"]) {
			return item, false, nil
		}
	} else if !conversationItemTypesWithRequiredID[typ] {
		delete(m, "id")
	}
	delete(m, "provider_data")

	cleanRaw, err := json.Marshal(m)
	if err != nil {
		return item, false, err
	}
	clean, err := agents.UnmarshalInputItem(cleanRaw)
	if err != nil {
		return item, false, err
	}
	return clean, true, nil
}

func isNonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// PopEntry implements agents.Session: it removes and returns the most recent
// item, or nil if the conversation is empty.
//
// The mutex is held across the list AND the delete: they are one removal
// (spec §2.5e2, selecting a row and removing it), and unserialized, two
// in-process pops both list the same newest item and one deletes what the
// other chose — or an append lands in between and the pop removes an item
// that is no longer the most recent. Cross-process races are inherent to the
// server API; the ones this SDK creates itself are not.
func (s *ConversationsSession) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.ensureID(ctx)
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
	entry, err := agents.NewItemEntry(item, agents.Source{})
	if err != nil {
		return nil, err
	}
	entry.ID = raw.ID
	return &entry, nil
}

// PopItem implements agents.ItemPopper. Every entry in a Conversations session
// IS a conversation item — the server holds Responses items and nothing else,
// so there are no banners, leaf moves or checkpoints to skip past and the two
// pops answer the same question here. A backend that can remove an entry
// offers both (spec §2.5e2): the flat, server-held store satisfies that with
// the trivial selection rather than the shared tree-aware one, which has no
// tree to consult.
func (s *ConversationsSession) PopItem(ctx context.Context) (*agents.SessionEntry, error) {
	return s.PopEntry(ctx)
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
