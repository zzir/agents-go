package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
)

// entryRow is one session entry.
//
// The whole entry is stored as JSON, with only the columns the queries need
// lifted out. That is the point of the change it replaced: the old messages
// table had a column per field the UI happened to want, so provenance, usage,
// diagnostics and the parent link — everything the SDK's entry model carries —
// had nowhere to go and was silently dropped on the way in.
type entryRow struct {
	bun.BaseModel `bun:"table:entries,alias:e"`

	ID        int64  `bun:"id,pk,autoincrement" json:"id"`
	SessionID string `bun:"session_id,notnull"  json:"session_id"`
	EntryID   string `bun:"entry_id,notnull"    json:"entry_id"`
	ParentID  string `bun:"parent_id"           json:"parent_id,omitempty"`
	Kind      string `bun:"kind,notnull"        json:"kind"`
	RunID     string `bun:"run_id"              json:"run_id,omitempty"`
	// Entry is the JSON of an agents.SessionEntry.
	Entry string `bun:"entry,type:text,notnull" json:"-"`
	// SourceModel records which model produced the entry, so replaying the
	// session against a different one can adapt or drop what that one would
	// reject — reasoning items above all.
	SourceModel string `bun:"source_model" json:"-"`
	// Compacted marks an entry the compaction pass folded away. It is a
	// soft delete: the row stays so the UI can still show what was folded.
	Compacted bool      `bun:"compacted"          json:"compacted,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

// EntryStore persists a server session's entries and serves them to the SDK.
//
// It implements agents.SessionStorage directly rather than adapting another
// shape, which is what removed the losses: an entry goes in as the runner
// produced it and comes back the same, display and provenance included.
type EntryStore struct {
	db        *bun.DB
	sessionID string
	runID     string
	// model is what this run targets. Entries produced by a different model are
	// adapted on the way out; see load.
	model string
}

// NewEntryStore returns storage for one session.
func NewEntryStore(db *bun.DB, sessionID string) *EntryStore {
	return &EntryStore{db: db, sessionID: sessionID}
}

// SetRunID stamps subsequent writes with the run that produced them, so the UI
// can group a transcript by turn and a reaper can find one run's rows.
func (s *EntryStore) SetRunID(runID string) { s.runID = runID }

// SetModel records the model this run targets, so history produced by another
// one is adapted rather than replayed verbatim into a backend that rejects it.
func (s *EntryStore) SetModel(model string) { s.model = model }

// Append implements agents.SessionStorage.
func (s *EntryStore) Append(ctx context.Context, entries ...agents.SessionEntry) error {
	return s.appendTo(ctx, s.db, entries...)
}

// appendTo is Append against a specific handle, so a caller that must append as
// part of a larger change — compaction marking rows folded and writing its
// checkpoint — can do both in one transaction instead of leaving a window where
// the session has one without the other.
func (s *EntryStore) appendTo(ctx context.Context, db bun.IDB, entries ...agents.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	leaf, err := s.leafIn(ctx, db)
	if err != nil {
		return err
	}
	seq, err := s.maxSeqIn(ctx, db)
	if err != nil {
		return err
	}
	prepared := agents.PrepareAppend(entries, leaf, seq, s.entryID)

	rows := make([]entryRow, 0, len(prepared))
	for i := range prepared {
		raw, err := json.Marshal(prepared[i])
		if err != nil {
			return fmt.Errorf("encoding entry %q: %w", prepared[i].ID, err)
		}
		rows = append(rows, entryRow{
			SessionID:   s.sessionID,
			SourceModel: s.model,
			EntryID:     prepared[i].ID,
			ParentID:    prepared[i].ParentID,
			Kind:        string(prepared[i].Kind),
			RunID:       s.runID,
			Entry:       string(raw),
			CreatedAt:   prepared[i].CreatedAt,
		})
	}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("appending %d entries: %w", len(rows), err)
	}
	return nil
}

func (s *EntryStore) entryID(seq int64) string {
	return fmt.Sprintf("%s-e%d", s.sessionID, seq)
}

// maxSeqIn returns the highest sequence number stored, which is the row count:
// rows are append-only and never renumbered.
func (s *EntryStore) maxSeqIn(ctx context.Context, db bun.IDB) (int64, error) {
	n, err := db.NewSelect().Model((*entryRow)(nil)).
		Where("session_id = ?", s.sessionID).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting entries: %w", err)
	}
	return int64(n), nil
}

// leafIn returns the branch tip: the last non-compacted entry's id.
func (s *EntryStore) leafIn(ctx context.Context, db bun.IDB) (string, error) {
	entries, err := s.loadIn(ctx, db, false)
	if err != nil {
		return "", err
	}
	return agents.LeafOf(entries), nil
}

// load reads the session's entries in append order. Excluding compacted rows is
// the read the RUN uses; including them is what the UI uses to show what was
// folded away.
func (s *EntryStore) load(ctx context.Context, includeCompacted bool) ([]agents.SessionEntry, error) {
	return s.loadIn(ctx, s.db, includeCompacted)
}

func (s *EntryStore) loadIn(ctx context.Context, db bun.IDB, includeCompacted bool) ([]agents.SessionEntry, error) {
	var rows []entryRow
	q := db.NewSelect().Model(&rows).
		Where("session_id = ?", s.sessionID).
		OrderExpr("id ASC")
	if !includeCompacted {
		q = q.Where("compacted = ?", false)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("loading entries: %w", err)
	}
	// Dropping an entry breaks the parent chain its children hang off, and a
	// broken chain ends the branch walk early — the session would come back
	// truncated at the drop rather than one entry lighter. skipped remaps a
	// dropped id onto its own parent so the survivors close the gap.
	skipped := map[string]string{}
	resolve := func(id string) string {
		for {
			next, ok := skipped[id]
			if !ok {
				return id
			}
			id = next
		}
	}

	out := make([]agents.SessionEntry, 0, len(rows))
	for i := range rows {
		var e agents.SessionEntry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			// One unreadable row must not make the whole session unloadable:
			// the rest of the conversation is still valid, and refusing to open
			// it would lose everything to one bad record.
			skipped[rows[i].EntryID] = rows[i].ParentID
			continue
		}
		// An item produced by another model may be one this backend rejects —
		// a reasoning block above all, whose shape and ids are provider
		// specific. Adapt it, or drop it when it cannot be adapted.
		if e.Kind == agents.EntryKindItem && s.model != "" &&
			rows[i].SourceModel != "" && rows[i].SourceModel != s.model {
			adapted := adaptForeignItemJSON(e.Item)
			if adapted == nil {
				skipped[e.ID] = e.ParentID
				continue
			}
			e.Item = adapted
		}
		e.ParentID = resolve(e.ParentID)
		e.Seq = int64(len(out) + 1)
		out = append(out, e)
	}
	return out, nil
}

// Entries implements agents.SessionStorage.
func (s *EntryStore) Entries(ctx context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	entries, err := s.load(ctx, false)
	if err != nil {
		return nil, err
	}
	return agents.PageEntries(entries, cur), nil
}

// Entry implements agents.SessionStorage.
func (s *EntryStore) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
	row := new(entryRow)
	err := s.db.NewSelect().Model(row).
		Where("session_id = ?", s.sessionID).
		Where("entry_id = ?", id).
		Scan(ctx)
	if err != nil {
		return nil, nil //nolint:nilerr // absent is not an error; the caller checks for nil
	}
	var e agents.SessionEntry
	if err := json.Unmarshal([]byte(row.Entry), &e); err != nil {
		return nil, fmt.Errorf("decoding entry %q: %w", id, err)
	}
	return &e, nil
}

// Metadata implements agents.SessionStorage.
func (s *EntryStore) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	n, err := s.db.NewSelect().Model((*entryRow)(nil)).
		Where("session_id = ?", s.sessionID).Count(ctx)
	if err != nil {
		return agents.SessionMetadata{}, err
	}
	return agents.SessionMetadata{ID: s.sessionID, EntryCount: n}, nil
}

// Clear implements agents.SessionStorage.
func (s *EntryStore) Clear(ctx context.Context) error {
	_, err := s.db.NewDelete().Model((*entryRow)(nil)).
		Where("session_id = ?", s.sessionID).Exec(ctx)
	return err
}

// PopEntry implements agents.EntryPopper: it removes the most recent entry, for
// an application undoing a turn.
//
// "Most recent" here means the most recent entry the model would have seen: an
// error banner or a folded-away entry is not something a person means to undo,
// and popping one would leave the turn it belongs to still in the history.
// Those rows stay where they are.
func (s *EntryStore) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	row := new(entryRow)
	if err := s.db.NewSelect().Model(row).
		Where("session_id = ?", s.sessionID).
		Where("compacted = ?", false).
		Where("kind = ?", string(agents.EntryKindItem)).
		OrderExpr("id DESC").Limit(1).Scan(ctx); err != nil {
		return nil, nil //nolint:nilerr // an empty session pops nothing
	}
	// Decode BEFORE deleting: a row that cannot be decoded is still the only
	// copy of what it holds, and reporting the failure after removing it would
	// lose it for good.
	var e agents.SessionEntry
	if err := json.Unmarshal([]byte(row.Entry), &e); err != nil {
		return nil, fmt.Errorf("decoding entry %q: %w", row.EntryID, err)
	}
	if _, err := s.db.NewDelete().Model((*entryRow)(nil)).
		Where("id = ?", row.ID).Exec(ctx); err != nil {
		return nil, err
	}
	return &e, nil
}

// AllEntries returns the session's entries INCLUDING compacted ones, for a UI
// that shows what a compaction folded away.
func (s *EntryStore) AllEntries(ctx context.Context) ([]agents.SessionEntry, error) {
	return s.load(ctx, true)
}

var (
	_ agents.SessionStorage = (*EntryStore)(nil)
	_ agents.EntryPopper    = (*EntryStore)(nil)
)

// EntryView is the REST shape of one entry.
//
// It is the stored entry plus the row id the cursor pages on. Nothing is
// re-derived here: Display, Source, Usage and Diagnostics come from what the
// runner wrote, which is the difference from the messages table this replaced —
// that one recomputed a display at read time from the raw item, and could only
// ever produce a worse version of what the SDK already knew.
type EntryView struct {
	ID       int64  `json:"id"`
	EntryID  string `json:"entry_id"`
	ParentID string `json:"parent_id,omitempty"`
	Kind     string `json:"kind"`
	RunID    string `json:"run_id,omitempty"`
	// Role is who produced the entry, from its recorded provenance rather than
	// re-parsed from the item. Provenance is not something to guess at.
	Role string `json:"role"`
	// Content is the readable text, for a renderer that wants one string.
	Content     string               `json:"content"`
	Display     *agents.ItemDisplay  `json:"display,omitempty"`
	Usage       *agents.RequestUsage `json:"usage,omitempty"`
	Diagnostics []agents.Diagnostic  `json:"diagnostics,omitempty"`
	Compacted   bool                 `json:"compacted,omitempty"`
	// OnPath reports whether this entry is on the session's ACTIVE branch. An
	// off-path entry is an abandoned attempt — still recorded, still offerable,
	// but not part of the conversation as it currently stands.
	OnPath bool `json:"on_path"`
	// Compaction is present on a checkpoint: what the pass folded away, so a
	// reader can collapse those entries under it and offer them back.
	Compaction *CompactionInfo `json:"compaction,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// CompactionInfo is a checkpoint's payload minus the retained tail, which is
// already in the session as ordinary entries — shipping it twice would double
// every kept turn in the client's timeline.
type CompactionInfo struct {
	// ExcludedIDs are the entries this checkpoint folded away. They are still
	// in the session, marked compacted; this is what lets a reader collapse
	// them under the checkpoint instead of leaving them loose in the history.
	ExcludedIDs  []string `json:"excluded_ids,omitempty"`
	TokensBefore int      `json:"tokens_before,omitempty"`
	TokensAfter  int      `json:"tokens_after,omitempty"`
}

// GetEntries returns a page of a session's entries, oldest first.
//
// With a limit it returns the NEWEST that many (still oldest-first) and the
// caller pages backwards with the smallest id it received — a cursor, because
// entries keep arriving and an offset would shift under a concurrent append.
//
// Update entries are folded into their targets here rather than shipped to the
// client. Folding is the SDK's rule and there should be one implementation of
// it; a client that re-derived it would be a second one, free to disagree. It
// happens over the whole session before the cursor is applied, because an
// update and the entry it amends need not land in the same page.
func (s *EntryStore) GetEntries(ctx context.Context, sessionID string, beforeID int64, limit int) ([]EntryView, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Where("session_id = ?", sessionID).
		OrderExpr("id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("getting entries: %w", err)
	}

	entries := make([]agents.SessionEntry, 0, len(rows))
	meta := make(map[string]entryRow, len(rows))
	for i := range rows {
		var e agents.SessionEntry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			continue
		}
		meta[e.ID] = rows[i]
		entries = append(entries, e)
	}

	onPath := activeBranch(entries)
	folded := agents.FoldUpdates(entries)
	views := make([]EntryView, 0, len(folded))
	for _, e := range folded {
		row := meta[e.ID]
		views = append(views, EntryView{
			ID:          row.ID,
			EntryID:     e.ID,
			ParentID:    e.ParentID,
			Kind:        string(e.Kind),
			RunID:       row.RunID,
			Role:        roleOf(e),
			Content:     contentOf(e),
			Display:     e.Display,
			Usage:       e.Usage,
			Diagnostics: e.Diagnostics,
			Compacted:   row.Compacted,
			OnPath:      onPath[e.ID],
			Compaction:  compactionInfoOf(e),
			CreatedAt:   e.CreatedAt,
		})
	}

	// The cursor applies to the folded list: paging on raw row ids would return
	// short pages wherever an update was folded away.
	if beforeID > 0 {
		cut := len(views)
		for i, v := range views {
			if v.ID >= beforeID {
				cut = i
				break
			}
		}
		views = views[:cut]
	}
	if limit > 0 && len(views) > limit {
		views = views[len(views)-limit:]
	}
	return views, nil
}

// roleOf maps an entry's provenance to the role a renderer groups by.
func roleOf(e agents.SessionEntry) string {
	switch e.Source.Type {
	case agents.SourceUser:
		return "user"
	case agents.SourceTool:
		return "tool"
	case agents.SourceHandoff:
		return "handoff"
	case agents.SourceCompaction:
		return "compaction"
	case agents.SourceErrorHandler, agents.SourceGuardrail:
		return "system"
	}
	if e.Kind == agents.EntryKindAnnotation {
		return "system"
	}
	return "assistant"
}

// contentOf returns the entry's readable text: the display's if the runner
// produced one, otherwise the entry's own.
func contentOf(e agents.SessionEntry) string {
	if e.Display != nil && e.Display.Text != "" {
		return e.Display.Text
	}
	// A checkpoint written by a compactor that set no display still has
	// something to show — the summary standing in for what it folded.
	if e.Kind == agents.EntryKindCompaction {
		if p, err := e.CompactionPayload(); err == nil {
			return p.Summary
		}
		return ""
	}
	if e.Kind != agents.EntryKindItem || len(e.Item) == 0 {
		return ""
	}
	return itemTextJSON(e.Item)
}

// compactionInfoOf reports what a checkpoint folded away, or nil for any other
// entry. The retained tail is deliberately not carried over: those entries are
// already in the session, and shipping them inside the checkpoint too would
// show every kept turn twice.
func compactionInfoOf(e agents.SessionEntry) *CompactionInfo {
	if e.Kind != agents.EntryKindCompaction {
		return nil
	}
	p, err := e.CompactionPayload()
	if err != nil {
		return nil
	}
	return &CompactionInfo{
		ExcludedIDs:  p.ExcludedIDs,
		TokensBefore: p.TokensBefore,
		TokensAfter:  p.TokensAfter,
	}
}

// Branch moves the session's active branch to entryID, so the next run
// continues from there.
//
// It appends a leaf entry rather than deleting anything: the abandoned attempt
// stays recorded, which is what makes "try that again differently" reversible
// and what lets the UI offer both versions.
func (s *EntryStore) Branch(ctx context.Context, sessionID, entryID string) error {
	store := &EntryStore{db: s.db, sessionID: sessionID, runID: s.runID}
	return agents.NewSession(store).Branch(ctx, entryID)
}

// Leaf returns the session's active branch tip.
func (s *EntryStore) Leaf(ctx context.Context, sessionID string) (string, error) {
	return (&EntryStore{db: s.db, sessionID: sessionID}).leafIn(ctx, s.db)
}

// activeBranch returns the ids on the current branch: the walk from the leaf up
// through parent links to the root.
//
// It deliberately does NOT use PathToLeaf, which stops at a compaction
// checkpoint. That stop is about what the MODEL reads; this is about which
// entries a person is currently looking at, and an entry folded by compaction
// is still on the branch they are on.
func activeBranch(entries []agents.SessionEntry) map[string]bool {
	byID := make(map[string]agents.SessionEntry, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}
	on := make(map[string]bool, len(entries))
	for id := agents.LeafOf(entries); id != ""; {
		e, ok := byID[id]
		if !ok || on[id] {
			break // a missing parent ends the walk; a repeat means a cycle
		}
		on[id] = true
		id = e.ParentID
	}
	return on
}

// AppendCallDisplayUpdate records an amendment to the display of whichever
// entry holds callID.
//
// It is an APPEND, and that is what removed the retry loop this replaced. A
// background task can finish before the turn that spawned it is persisted, so
// the old read-modify-write had to scan for a row that did not exist yet and
// try again for thirty seconds. An update entry may be stored before its
// target; projection associates them by call id afterwards, so there is nothing
// to wait for.
func (s *EntryStore) AppendCallDisplayUpdate(ctx context.Context, sessionID, callID string, display agents.ItemDisplay) error {
	e, err := agents.NewCallUpdateEntry(callID, display)
	if err != nil {
		return err
	}
	store := &EntryStore{db: s.db, sessionID: sessionID, runID: s.runID}
	return store.Append(ctx, e)
}

// AppendAnnotation records something shown to people but never sent to the
// model: an error banner, a cancellation notice.
func (s *EntryStore) AppendAnnotation(ctx context.Context, sessionID, runID, text string) error {
	store := &EntryStore{db: s.db, sessionID: sessionID, runID: runID}
	return store.Append(ctx, agents.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayError, Text: text},
		agents.Source{Type: agents.SourceErrorHandler},
	))
}

// forkEntriesTx copies a prefix of src's entries into dst.
//
// Entry ids are rewritten to the destination's namespace and parent links are
// remapped alongside them, so the fork is a self-consistent tree rather than
// one pointing back at entries in another session. Copying the ids verbatim
// would make the two sessions' entries indistinguishable by id, which every
// lookup here keys on.
func forkEntriesTx(ctx context.Context, tx bun.Tx, srcSessionID, dstSessionID string, upToID int64, exclusive bool) ([]string, error) {
	var rows []entryRow
	q := tx.NewSelect().Model(&rows).
		Where("session_id = ?", srcSessionID).
		OrderExpr("id ASC")
	if upToID > 0 {
		if exclusive {
			q = q.Where("id < ?", upToID)
		} else {
			q = q.Where("id <= ?", upToID)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("fork entries read: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	var runIDs []string
	seen := map[string]bool{}
	remap := make(map[string]string, len(rows))
	now := time.Now().UTC()
	for i := range rows {
		if rid := rows[i].RunID; rid != "" && !seen[rid] {
			seen[rid] = true
			runIDs = append(runIDs, rid)
		}
		newID := fmt.Sprintf("%s-e%d", dstSessionID, i+1)
		remap[rows[i].EntryID] = newID

		var e agents.SessionEntry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			continue
		}
		e.ID = newID
		e.ParentID = remap[e.ParentID] // "" for a root maps to "" — the zero value
		e.Seq = int64(i + 1)
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("fork entries encode: %w", err)
		}
		rows[i].ID = 0
		rows[i].SessionID = dstSessionID
		rows[i].EntryID = newID
		rows[i].ParentID = e.ParentID
		rows[i].Entry = string(raw)
		rows[i].CreatedAt = now
	}
	if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return nil, fmt.Errorf("fork entries write: %w", err)
	}
	return runIDs, nil
}

// ForkSession atomically creates dst and copies src's entries into it in one
// transaction, so a failure never leaves an orphaned empty session behind.
func (s *EntryStore) ForkSession(ctx context.Context, dst *Session, srcSessionID string, upToID int64, exclusive bool) ([]string, error) {
	var runIDs []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Confirm the source still exists inside the tx: a concurrent delete
		// between the handler's read and here would otherwise yield an empty
		// entry set and a bogus empty fork returned as success.
		exists, err := tx.NewSelect().Model((*Session)(nil)).Where("id = ?", srcSessionID).Exists(ctx)
		if err != nil {
			return fmt.Errorf("fork source check: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		now := time.Now().UTC()
		dst.CreatedAt, dst.UpdatedAt = now, now
		if _, err := tx.NewInsert().Model(dst).Exec(ctx); err != nil {
			return fmt.Errorf("fork create session: %w", err)
		}
		var e error
		runIDs, e = forkEntriesTx(ctx, tx, srcSessionID, dst.ID, upToID, exclusive)
		return e
	})
	if err != nil {
		return nil, err
	}
	return runIDs, nil
}

// DeleteBySession removes every entry of a session.
func (s *EntryStore) DeleteBySession(ctx context.Context, sessionID string) error {
	_, err := s.db.NewDelete().Model((*entryRow)(nil)).
		Where("session_id = ?", sessionID).Exec(ctx)
	return err
}
