package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// GetMessages returns messages for sessionID ordered oldest first. limit > 0
// selects the newest `limit` rows (optionally only those with id < beforeID,
// the backwards-pagination cursor); limit <= 0 returns everything. Rows
// written before the display column existed get their display projection
// derived from the stored item on the way out, so the frontend never has to
// parse wire-format item JSON.
func (s *MessageStore) GetMessages(ctx context.Context, sessionID string, beforeID int64, limit int) ([]Message, error) {
	var msgs []Message
	q := s.db.NewSelect().Model(&msgs).
		Where("session_id = ?", sessionID)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if limit > 0 {
		// Newest page first, then flip back to chronological order.
		q = q.OrderExpr("id DESC").Limit(limit)
	} else {
		q = q.OrderExpr("id ASC")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("getting messages: %w", err)
	}
	if limit > 0 {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	for i := range msgs {
		if msgs[i].Item == "" {
			continue
		}
		if len(msgs[i].Display) == 0 {
			msgs[i].Display = deriveDisplay([]byte(msgs[i].Item))
		}
		// Re-derive an empty content projection: rows written while the
		// extractor didn't understand the item's shape (e.g. "text" content
		// parts) heal on read once the extractor learns it.
		if msgs[i].Content == "" && msgs[i].Kind != MessageKindAnnotation {
			_, msgs[i].Content = extractRoleContent([]byte(msgs[i].Item))
		}
	}
	return msgs, nil
}

// SessionAdapter bridges the store's message table to the agents.Session
// interface so the SDK runner can load/save conversation history.
type SessionAdapter struct {
	db        *bun.DB
	sessionID string
	runID     string
	model     string
}

// NewSessionAdapter returns a SessionAdapter bound to db and sessionID.
func NewSessionAdapter(db *bun.DB, sessionID string) *SessionAdapter {
	return &SessionAdapter{db: db, sessionID: sessionID}
}

// SetRunID stamps all subsequent AddItems calls with the given run ID.
func (a *SessionAdapter) SetRunID(runID string) { a.runID = runID }

// SetModel records the model this run targets. It stamps new item rows as
// their source model and drives the replay policy in GetItems: items produced
// by a different model are adapted (ids stripped) or dropped (reasoning).
func (a *SessionAdapter) SetModel(model string) { a.model = model }

// NewItemMessage builds the canonical Message row for one replayable
// conversation item. All writers of kind="item" rows must go through here (or
// NewItemMessageRaw) so the table never accumulates provider-specific shapes.
func NewItemMessage(sessionID, runID, sourceModel string, item agents.TResponseInputItem) (Message, error) {
	raw, err := agents.MarshalInputItem(item)
	if err != nil {
		return Message{}, fmt.Errorf("marshaling item: %w", err)
	}
	return NewItemMessageRaw(sessionID, runID, sourceModel, raw), nil
}

// NewItemMessageRaw is NewItemMessage for callers that already hold the item's
// wire JSON. The JSON is normalized at write time and the denormalized
// role/content/display projections are derived from the normalized form.
func NewItemMessageRaw(sessionID, runID, sourceModel string, raw []byte) Message {
	norm := NormalizeItemJSON(raw)
	role, content := extractRoleContent(norm)
	return Message{
		SessionID:   sessionID,
		RunID:       runID,
		Kind:        MessageKindItem,
		Role:        role,
		Content:     content,
		Display:     deriveDisplay(norm),
		Item:        string(norm),
		SourceModel: sourceModel,
		CreatedAt:   time.Now().UTC(),
	}
}

// NewAnnotationMessage builds a display-only row (error text, partial
// reasoning from a cancelled run). It has no Item and is never replayed.
func NewAnnotationMessage(sessionID, runID, role, content string) Message {
	return Message{
		SessionID: sessionID,
		RunID:     runID,
		Kind:      MessageKindAnnotation,
		Role:      role,
		Content:   content,
		CreatedAt: time.Now().UTC(),
	}
}

// Entries implements agents.SessionStorage.
//
// It returns item entries only. Annotation rows are already excluded from the
// model's view by the query below; surfacing them here as annotation entries is
// part of converging this adapter on the entry model (plan1 P4), not of moving
// the interface.
func (a *SessionAdapter) Entries(ctx context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	limit := 0
	if cur.Limit < 0 {
		limit = -cur.Limit
	}
	items, err := a.getItems(ctx, limit)
	if err != nil {
		return nil, err
	}
	entries, err := agents.NewItemEntries(items, agents.Source{})
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Seq = int64(i + 1)
	}
	return agents.PageEntries(entries, agents.Cursor{AfterSeq: cur.AfterSeq}), nil
}

// Metadata implements agents.SessionStorage.
func (a *SessionAdapter) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	n, err := a.db.NewSelect().Model((*Message)(nil)).
		Where("session_id = ?", a.sessionID).Count(ctx)
	if err != nil {
		return agents.SessionMetadata{}, err
	}
	return agents.SessionMetadata{ID: a.sessionID, EntryCount: n}, nil
}

// Entry implements agents.SessionStorage.
func (a *SessionAdapter) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
	entries, err := a.Entries(ctx, agents.Cursor{})
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

// Append implements agents.SessionStorage. Item entries are stored as message
// rows; other kinds are rejected rather than dropped, because this adapter owns
// its own store and could represent them — a silent loss here would be a bug,
// not a limitation.
func (a *SessionAdapter) Append(ctx context.Context, entries ...agents.SessionEntry) error {
	items := make([]agents.TResponseInputItem, 0, len(entries))
	for _, e := range entries {
		if e.Kind != "" && e.Kind != agents.EntryKindItem {
			return fmt.Errorf("session adapter: cannot store a %q entry yet", e.Kind)
		}
		item, err := e.InputItem()
		if err != nil {
			return err
		}
		items = append(items, item)
	}
	return a.addItems(ctx, items)
}

// PopEntry implements agents.EntryPopper.
func (a *SessionAdapter) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	item, err := a.popItem(ctx)
	if err != nil || item == nil {
		return nil, err
	}
	e, err := agents.NewItemEntry(*item, agents.Source{})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// getItems returns the session's stored input items oldest first; a positive
// limit returns only the most recent limit items (still in chronological order).
func (a *SessionAdapter) getItems(ctx context.Context, limit int) ([]agents.TResponseInputItem, error) {
	q := a.db.NewSelect().Model((*Message)(nil)).
		Column("item", "source_model", "role").
		Where("session_id = ?", a.sessionID).
		Where("compacted = ?", false).
		// NULL-tolerant: rows written before the kind column existed replay as items.
		Where("kind IS NULL OR kind <> ?", MessageKindAnnotation)

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

	// Compaction summaries are written after the rows they summarize, so by id
	// they sort after the keep-window. The model must see them first — a
	// summary describes the *older* history — matching SlidingWindowSession's
	// [summary, kept...] ordering.
	var summaries, items []agents.TResponseInputItem
	for _, m := range msgs {
		if m.Item == "" || m.Item == "{}" || m.Item == "null" {
			continue
		}
		raw := []byte(m.Item)
		if a.model != "" && m.SourceModel != "" && m.SourceModel != a.model {
			raw = adaptForeignItemJSON(raw)
			if raw == nil {
				continue
			}
		}
		// Items are normalized at write time; normalizing again here keeps
		// rows written by older versions replayable.
		item, err := agents.UnmarshalInputItem(NormalizeItemJSON(raw))
		if err != nil {
			continue
		}
		if m.Role == "compaction" {
			summaries = append(summaries, item)
		} else {
			items = append(items, item)
		}
	}
	return append(summaries, items...), nil
}

// adaptForeignItemJSON adapts an item produced by a different model for
// replay: reasoning items are dropped entirely (their shape and ids are
// provider-specific and rejected by other backends), and provider-assigned
// item ids are stripped so the target backend does not try to resolve them.
// Returns nil when the item must be skipped.
func adaptForeignItemJSON(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	var typ string
	if t, ok := m["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	if typ == "reasoning" {
		return nil
	}
	if _, ok := m["id"]; !ok {
		return raw
	}
	delete(m, "id")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// NormalizeItemJSON rewrites a stored item for replay compatibility with
// strict Responses-API backends that require message `content` to always be an
// array: user/system/developer messages stored with bare-string content (the
// shape the SDK writes for plain-text input) get the string wrapped in a
// one-part input_text array, and a literal `"content": null` (seen on some
// backends' reasoning items) is dropped entirely. Items already in array form
// pass through untouched.
func NormalizeItemJSON(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	c, ok := m["content"]
	if !ok {
		return raw
	}
	if string(c) == "null" {
		delete(m, "content")
		out, err := json.Marshal(m)
		if err != nil {
			return raw
		}
		return out
	}
	var text string
	if json.Unmarshal(c, &text) != nil {
		return raw // already an array/object — leave as-is
	}
	var role string
	if r, ok := m["role"]; ok {
		_ = json.Unmarshal(r, &role)
	}
	if role != "user" && role != "system" && role != "developer" {
		return raw
	}
	parts, err := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
	if err != nil {
		return raw
	}
	m["content"] = parts
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// AddItems appends items to the session, persisting each as a Message.
func (a *SessionAdapter) addItems(ctx context.Context, items []agents.TResponseInputItem) error {
	if len(items) == 0 {
		return nil
	}
	msgs := make([]Message, 0, len(items))
	for _, item := range items {
		m, err := NewItemMessage(a.sessionID, a.runID, a.model, item)
		if err != nil {
			return fmt.Errorf("session adapter add items: %w", err)
		}
		msgs = append(msgs, m)
	}
	if _, err := a.db.NewInsert().Model(&msgs).Exec(ctx); err != nil {
		return fmt.Errorf("session adapter add items: %w", err)
	}
	return nil
}

// deriveDisplay extracts the structured fields the UI renders for tool-call
// rows, so the frontend reads typed display data instead of wire JSON.
func deriveDisplay(raw []byte) json.RawMessage {
	var probe struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Output    any    `json:"output"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	switch probe.Type {
	case "function_call":
		d, _ := json.Marshal(map[string]string{
			"call_id": probe.CallID, "name": probe.Name, "arguments": probe.Arguments,
		})
		return d
	case "function_call_output":
		out := ""
		switch v := probe.Output.(type) {
		case string:
			out = v
		case nil:
		default:
			b, _ := json.Marshal(v)
			out = string(b)
		}
		d, _ := json.Marshal(map[string]string{"call_id": probe.CallID, "output": out})
		return d
	}
	return nil
}

func extractRoleContent(raw []byte) (role, content string) {
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
	case probe.Type == "reasoning":
		return "reasoning", extractReasoningText(raw)
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
			// "text" is the part type some Responses-compatible backends
			// (vLLM and friends) emit instead of output_text.
			if (p.Type == "input_text" || p.Type == "output_text" || p.Type == "text") && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// extractReasoningText pulls the thinking text out of a Responses reasoning
// item: the standard `summary` array, falling back to the `content` array
// some Responses-compatible backends use for raw reasoning text.
func extractReasoningText(raw []byte) string {
	var probe struct {
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	out := ""
	for _, s := range probe.Summary {
		if s.Text == "" {
			continue
		}
		if out != "" {
			out += "\n\n"
		}
		out += s.Text
	}
	if out == "" {
		for _, c := range probe.Content {
			if c.Text == "" {
				continue
			}
			if out != "" {
				out += "\n\n"
			}
			out += c.Text
		}
	}
	return out
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

// PopItem removes and returns the most recent REPLAYABLE item, or (nil, nil)
// when the session holds no such item — the contract the SDK's Session
// interface requires for an "empty" session. It matches GetItems' filter
// (non-compacted, non-annotation, non-empty/{}/null item) so it never pops and
// deletes a UI-only annotation row or a soft-deleted/compacted row, which would
// corrupt history or fail to deserialize.
func (a *SessionAdapter) popItem(ctx context.Context) (*agents.TResponseInputItem, error) {
	var item agents.TResponseInputItem
	err := a.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var msg Message
		if err := tx.NewSelect().Model(&msg).
			Where("session_id = ?", a.sessionID).
			Where("compacted = ?", false).
			Where("kind IS NULL OR kind <> ?", MessageKindAnnotation).
			// Empty / {} / null items are unreplayable placeholders GetItems
			// skips too; excluding them here keeps PopItem from deleting a row
			// it could never deserialize.
			Where("item <> ''").
			Where("item <> '{}'").
			Where("item <> 'null'").
			OrderExpr("id DESC").
			Limit(1).
			Scan(ctx); err != nil {
			return err
		}
		// Deserialize BEFORE deleting: if the item can't be decoded, returning
		// the error rolls the tx back so the row survives instead of being lost.
		parsed, err := agents.UnmarshalInputItem(NormalizeItemJSON([]byte(msg.Item)))
		if err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*Message)(nil)).
			Where("id = ?", msg.ID).
			Exec(ctx); err != nil {
			return err
		}
		item = parsed
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // empty session: the Session contract wants (nil, nil)
		}
		return nil, fmt.Errorf("session adapter pop item: %w", err)
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

// forkMessagesTx copies src's messages into dstSessionID within tx and returns
// the deduplicated run ids. Shared by ForkMessages and ForkSession so both run
// the read and the write in one transaction.
func forkMessagesTx(ctx context.Context, tx bun.Tx, srcSessionID, dstSessionID string, upToMessageID int64, exclusive bool) ([]string, error) {
	var msgs []Message
	q := tx.NewSelect().Model(&msgs).
		Where("session_id = ?", srcSessionID).
		OrderExpr("id ASC")
	if upToMessageID > 0 {
		if exclusive {
			q = q.Where("id < ?", upToMessageID)
		} else {
			q = q.Where("id <= ?", upToMessageID)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("fork messages read: %w", err)
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	var runIDs []string
	seen := map[string]struct{}{}
	now := time.Now().UTC()
	for i := range msgs {
		if rid := msgs[i].RunID; rid != "" {
			if _, ok := seen[rid]; !ok {
				seen[rid] = struct{}{}
				runIDs = append(runIDs, rid)
			}
		}
		msgs[i].ID = 0
		msgs[i].SessionID = dstSessionID
		msgs[i].CreatedAt = now
	}
	if _, err := tx.NewInsert().Model(&msgs).Exec(ctx); err != nil {
		return nil, fmt.Errorf("fork messages write: %w", err)
	}
	return runIDs, nil
}

// ForkMessages copies messages from srcSessionID to dstSessionID. When
// upToMessageID > 0, only messages with id up to that ID are copied (inclusive
// by default, exclusive when exclusive is true); otherwise all messages are
// copied. Returns the deduplicated run IDs found in the copied messages. The
// read and write run in one transaction — a consistent snapshot with no
// half-written fork.
func (s *MessageStore) ForkMessages(ctx context.Context, srcSessionID, dstSessionID string, upToMessageID int64, exclusive bool) ([]string, error) {
	var runIDs []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var e error
		runIDs, e = forkMessagesTx(ctx, tx, srcSessionID, dstSessionID, upToMessageID, exclusive)
		return e
	})
	if err != nil {
		return nil, err
	}
	return runIDs, nil
}

// ForkSession atomically creates dst and copies src's messages into it in a
// single transaction, so a failure (or a cancelled request) never leaves an
// orphaned empty session behind — the gap the handler's separate
// create-then-copy left open.
func (s *MessageStore) ForkSession(ctx context.Context, dst *Session, srcSessionID string, upToMessageID int64, exclusive bool) ([]string, error) {
	var runIDs []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Confirm the source still exists inside the tx: a concurrent delete
		// between the handler's read and here would otherwise yield an empty
		// message set and a bogus empty fork returned as success.
		exists, err := tx.NewSelect().Model((*Session)(nil)).Where("id = ?", srcSessionID).Exists(ctx)
		if err != nil {
			return fmt.Errorf("fork source check: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		now := time.Now().UTC()
		dst.CreatedAt = now
		dst.UpdatedAt = now
		if _, err := tx.NewInsert().Model(dst).Exec(ctx); err != nil {
			return fmt.Errorf("fork create session: %w", err)
		}
		var e error
		runIDs, e = forkMessagesTx(ctx, tx, srcSessionID, dst.ID, upToMessageID, exclusive)
		return e
	})
	if err != nil {
		return nil, err
	}
	return runIDs, nil
}

// AddAnnotation inserts a display-only annotation row (never replayed to the
// model) for the given session and run.
func (s *MessageStore) AddAnnotation(ctx context.Context, sessionID, runID, role, content string) error {
	m := NewAnnotationMessage(sessionID, runID, role, content)
	if _, err := s.db.NewInsert().Model(&m).Exec(ctx); err != nil {
		return fmt.Errorf("adding annotation: %w", err)
	}
	return nil
}

// DeleteBySession removes all messages belonging to sessionID.
func (s *MessageStore) DeleteBySession(ctx context.Context, sessionID string) error {
	_, err := s.db.NewDelete().Model((*Message)(nil)).
		Where("session_id = ?", sessionID).
		Exec(ctx)
	return err
}

var (
	_ agents.SessionStorage = (*SessionAdapter)(nil)
	_ agents.EntryPopper    = (*SessionAdapter)(nil)
)

// terminalTaskStatuses are the spawn-card task_status values that must never be
// rolled back once shown. They mirror the terminal set Task.Finalize enforces
// on the durable row (taskTerminalSet / protocol.Task{Completed,Failed,
// Cancelled}); keeping the display projection first-terminal-wins stops a late
// or reordered patch from reverting a finished card.
var terminalTaskStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"cancelled": true,
}

// isTerminalTaskStatus reports whether v (a display's task_status field, decoded
// as any) is one of the frozen terminal states.
func isTerminalTaskStatus(v any) bool {
	s, ok := v.(string)
	return ok && terminalTaskStatuses[s]
}

// sameTaskStatus reports whether two decoded task_status values are the same
// string (so an idempotent re-patch of a terminal status still applies).
func sameTaskStatus(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	return aok && bok && as == bs
}

// PatchToolCallDisplay merges patch into the display projection of the
// tool_call row with the given call id in sessionID. Used when a background
// task ends: the spawn tool call's card gets the terminal status/summary, so a
// reload rebuilds the task card from the row (the hub's RunInfo is GC'd
// minutes after the run). The wire JSON in Item is untouched — display is a
// UI projection, not replay truth.
func (s *MessageStore) PatchToolCallDisplay(ctx context.Context, sessionID, callID string, patch map[string]any) error {
	// Read → merge → write must be one transaction: two patches racing on the
	// same card (a fast status change and its retry, or two concurrent
	// finalizers) would otherwise each read the pre-merge display and the later
	// UPDATE would clobber the earlier one's fields. On SQLite's single writer
	// the tx serializes them, so every patch merges onto the previous result.
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Scan every tool_call row of the session (newest first): a spawn card can
		// be arbitrarily old by the time its task finishes, so a bounded window
		// would silently drop the terminal patch.
		var rows []Message
		if err := tx.NewSelect().Model(&rows).
			Column("id", "display").
			Where("session_id = ?", sessionID).
			Where("role = ?", "tool_call").
			OrderExpr("id DESC").
			Scan(ctx); err != nil {
			return fmt.Errorf("finding tool call %s: %w", callID, err)
		}
		for _, row := range rows {
			var d map[string]any
			if len(row.Display) == 0 || json.Unmarshal(row.Display, &d) != nil {
				continue
			}
			if d["call_id"] != callID {
				continue
			}
			// A terminal task_status is frozen (first terminal wins, mirroring
			// Task.Finalize): a late or reordered patch carrying an earlier
			// non-terminal status — or a competing terminal — must not roll the
			// card back. Such a patch is stale; applying it is a no-op. An
			// idempotent re-patch of the same terminal status still falls
			// through so it can carry an updated summary.
			if incoming, ok := patch["task_status"]; ok {
				if cur := d["task_status"]; isTerminalTaskStatus(cur) && !sameTaskStatus(cur, incoming) {
					return nil
				}
			}
			for k, v := range patch {
				d[k] = v
			}
			merged, err := json.Marshal(d)
			if err != nil {
				return fmt.Errorf("merging display for %s: %w", callID, err)
			}
			if _, err := tx.NewUpdate().Model((*Message)(nil)).
				Set("display = ?", string(merged)).
				Where("id = ?", row.ID).
				Exec(ctx); err != nil {
				return fmt.Errorf("patching display for %s: %w", callID, err)
			}
			return nil
		}
		return fmt.Errorf("tool call %s not found in session %s: %w", callID, sessionID, ErrNotFound)
	})
}
