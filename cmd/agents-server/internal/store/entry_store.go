package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/session"
)

// entryRow is one session entry: the whole entry as JSON, with only the
// columns the queries need lifted out.
type entryRow struct {
	bun.BaseModel `bun:"table:entries,alias:e"`

	ID        string `bun:"id,pk,type:uuid"     json:"id"`
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	// Gen is the session generation these entries belong to; see
	// session.Ref. Empty is the direct scope, not a wildcard.
	Gen string `bun:"gen,notnull" json:"-"`
	// Seq is the entry's session-local cursor position, allocated by
	// session.PrepareAppend — not the row id.
	Seq      int64  `bun:"seq,notnull" json:"-"`
	EntryID  string `bun:"entry_id,notnull"    json:"entry_id"`
	ParentID string `bun:"parent_id"           json:"parent_id,omitempty"`
	Kind     string `bun:"kind,notnull"        json:"kind"`
	RunID    string `bun:"run_id,nullzero,type:uuid" json:"run_id,omitempty"`
	// Entry is the JSON of an session.Entry.
	Entry string `bun:"entry,type:text,notnull" json:"-"`
	// SourceModel records which model produced the entry, so a replay against
	// another model can adapt or drop what it would reject.
	SourceModel string `bun:"source_model" json:"-"`
	// Usage (RequestUsage as JSON) and EstTokens (CharEstimator's size) are
	// lifted out so a reader can size a session by row count, not bytes.
	Usage     string `bun:"usage,nullzero" json:"-"`
	EstTokens int    `bun:"est_tokens"     json:"-"`
	// Compacted marks an entry the compaction pass folded away. It is a
	// soft delete: the row stays so the UI can still show what was folded.
	Compacted bool      `bun:"compacted"          json:"compacted,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

// BeforeAppendModel mints the id on insert; bun invokes it on insert and update.
func (r *entryRow) BeforeAppendModel(_ context.Context, q bun.Query) error {
	if _, ok := q.(*bun.InsertQuery); ok && r.ID == "" {
		r.ID = NewTimeID() // append-heavy table: time-ordered ids (see NewTimeID)
	}
	return nil
}

// appendPointRow is where one session stands: the branch tip and the highest
// sequence number it holds. Not a cache: every path that moves either writes
// it in the same transaction — invariant 59; foldAppendPointIn is the definition.
type appendPointRow struct {
	bun.BaseModel `bun:"table:append_points,alias:ap"`

	SessionID string `bun:"session_id,pk,type:uuid"`
	// Gen is the generation this point belongs to — part of the key, as it is
	// part of every other address of an entry row (see EntryStore.scoped).
	Gen string `bun:"gen,pk"`
	// LeafEntryID is the tip the next append links to; empty starts a root.
	LeafEntryID string `bun:"leaf_entry_id,notnull"`
	// LastSeq is the highest sequence number the session HOLDS, which a
	// removal lowers; SeqFor takes the clock over this floor, so that is safe.
	LastSeq int64 `bun:"last_seq,notnull"`
}

// EntryStore persists a server session's entries and serves them to the SDK
// as a session.Storage, display and provenance included.
type EntryStore struct {
	db    *bun.DB
	ref   session.Ref
	runID string
	// model is what this run targets. Entries produced by a different model are
	// adapted on the way out; see load.
	model string
}

// NewEntryStoreFor returns storage addressed by ref, so the generation is
// never resolved a second time (an id can be deleted and recreated between).
func NewEntryStoreFor(db *bun.DB, ref session.Ref) *EntryStore {
	return &EntryStore{db: db, ref: ref}
}

// NewSharedEntryStore returns a handle that owns no session, for the methods
// that take a ref explicitly.
func NewSharedEntryStore(db *bun.DB) *EntryStore {
	return &EntryStore{db: db}
}

// RefFor resolves the generation currently answering to a session id, for a
// caller that holds a shared handle rather than the database.
func (s *EntryStore) RefFor(ctx context.Context, sessionID string) (session.Ref, error) {
	return RefFor(ctx, s.db, sessionID)
}

// forRef returns a handle for another session, carrying this one's run and
// model.
func (s *EntryStore) forRef(ref session.Ref) *EntryStore {
	return &EntryStore{db: s.db, ref: ref, runID: s.runID, model: s.model}
}

// scoped narrows a query to this session; every read and write of entry rows
// goes through it, so the generation is part of the address.
func (s *EntryStore) scoped(q *bun.SelectQuery) *bun.SelectQuery {
	return q.Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen)
}

// SessionIsPlanning reports whether the session should START its next run in
// the planning phase: a single-row read of the materialized column
// (Session.Planning). The state belongs to the SESSION, not a run.
func (s *EntryStore) SessionIsPlanning(ctx context.Context, ref session.Ref) (bool, error) {
	var planning bool
	err := s.db.NewSelect().Model((*Session)(nil)).Column("planning").
		Where("id = ?", ref.ID).Scan(ctx, &planning)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // a session that no longer exists plans for nobody
		}
		return false, fmt.Errorf("reading plan phase for session %s: %w", ref.ID, err)
	}
	return planning, nil
}

// SetSessionPlanning writes the session's plan phase; last write wins, the
// approved submit_plan's unlock included (see armPlanUnlock).
func (s *EntryStore) SetSessionPlanning(ctx context.Context, ref session.Ref, planning bool) error {
	_, err := s.db.NewUpdate().Model((*Session)(nil)).
		Set("planning = ?", planning).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", ref.ID).Exec(ctx)
	if err != nil {
		return fmt.Errorf("writing plan phase for session %s: %w", ref.ID, err)
	}
	return nil
}

// RunHasItems reports whether the run persisted any replayable item entry,
// so a fallback record of a dead run does not duplicate what per-turn persistence saved.
func (s *EntryStore) RunHasItems(ctx context.Context, runID string) (bool, error) {
	exists, err := s.scoped(s.db.NewSelect().Model((*entryRow)(nil))).
		Where("run_id = ?", runID).
		Where("kind = ?", string(session.EntryKindItem)).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("checking run %s for persisted items: %w", runID, err)
	}
	return exists, nil
}

// RefFor resolves the generation currently answering to a session id. Only
// "no such session" is absence; a failure to look is an error (spec §2.5e2).
func RefFor(ctx context.Context, db bun.IDB, sessionID string) (session.Ref, error) {
	var row Session
	err := db.NewSelect().Model(&row).Column("gen").
		Where("id = ?", sessionID).Limit(1).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return session.Ref{}, fmt.Errorf("session %s: %w", sessionID, ErrNotFound)
	case err != nil:
		return session.Ref{}, fmt.Errorf("resolving session %s: %w", sessionID, err)
	}
	return session.Ref{ID: sessionID, Gen: row.Gen}, nil
}

// SetRunID stamps subsequent writes with the run that produced them, so the UI
// can group a transcript by turn and a reaper can find one run's rows.
func (s *EntryStore) SetRunID(runID string) { s.runID = runID }

// SetModel records the model this run targets, so history produced by another
// one is adapted rather than replayed verbatim into a backend that rejects it.
func (s *EntryStore) SetModel(model string) { s.model = model }

// Append implements session.Storage. The append point is read and written
// under the session row's lock (lockSessionIn) — spec §2.5e2.
func (s *EntryStore) Append(ctx context.Context, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.appendTo(ctx, tx, entries...)
	})
}

// appendTo is Append against a specific handle, so compaction can fold rows
// and write its checkpoint in one transaction.
func (s *EntryStore) appendTo(ctx context.Context, db bun.IDB, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := s.lockSessionIn(ctx, db); err != nil {
		return err
	}
	at, err := s.appendPointIn(ctx, db)
	if err != nil {
		return err
	}
	prepared := session.PrepareAppend(entries, at)

	rows := make([]entryRow, 0, len(prepared))
	for i := range prepared {
		raw, err := json.Marshal(prepared[i])
		if err != nil {
			return fmt.Errorf("encoding entry %q: %w", prepared[i].ID, err)
		}
		usage, size, err := liftedFields(prepared[i])
		if err != nil {
			return fmt.Errorf("entry %q: %w", prepared[i].ID, err)
		}
		rows = append(rows, entryRow{
			SessionID:   s.ref.ID,
			Gen:         s.ref.Gen,
			Seq:         prepared[i].Seq,
			SourceModel: s.model,
			EntryID:     prepared[i].ID,
			ParentID:    prepared[i].ParentID,
			Kind:        string(prepared[i].Kind),
			RunID:       s.runID,
			Entry:       string(raw),
			Usage:       usage,
			EstTokens:   size,
			CreatedAt:   prepared[i].CreatedAt,
		})
	}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("appending %d entries: %w", len(rows), err)
	}
	if err := writeAppendPoint(ctx, db, s.ref, appendPointAfter(at, prepared)); err != nil {
		return err
	}
	return s.touchSessionIn(ctx, db)
}

// liftedFields are what an entry carries in columns beside its body: its usage
// as JSON (empty when it carries none) and CharEstimator's size for it.
func liftedFields(e session.Entry) (usageJSON string, estTokens int, err error) {
	if e.Usage != nil {
		raw, err := json.Marshal(e.Usage)
		if err != nil {
			return "", 0, fmt.Errorf("encoding usage: %w", err)
		}
		usageJSON = string(raw)
	}
	return usageJSON, compaction.CharEstimator{}.Estimate(e), nil
}

// appendPointAfter reports where the session stands once prepared is written
// (a leaf moves the tip to its target, anything else becomes the tip), seeded with the previous point.
func appendPointAfter(at session.AppendPoint, prepared []session.Entry) session.AppendPoint {
	for _, e := range prepared {
		at.LastSeq = max(at.LastSeq, e.Seq)
		if e.Kind == session.EntryKindLeaf {
			if p, err := e.LeafPayload(); err == nil {
				at.Leaf = p.TargetID
			}
			continue
		}
		at.Leaf = e.ID
	}
	return at
}

// lockSessionIn takes the session row's lock FIRST — the order every
// append-point write and deleteSessionRows share (PostgreSQL only). A repo session whose row is gone fails the write (spec §2.5e2).
func (s *EntryStore) lockSessionIn(ctx context.Context, db bun.IDB) error {
	if s.ref.Gen == "" || db.Dialect().Name() != dialect.PG {
		return nil
	}
	err := db.NewSelect().Model((*Session)(nil)).Column("id").
		Where("id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		For("UPDATE").Scan(ctx, new(string))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("session %s: %w", s.ref.ID, session.ErrNotFound)
	case err != nil:
		return fmt.Errorf("locking session %s: %w", s.ref.ID, err)
	}
	return nil
}

// touchSessionIn records that the session changed (a listing sorts by that),
// on the same handle as the change. No session row is nothing to record.
func (s *EntryStore) touchSessionIn(ctx context.Context, db bun.IDB) error {
	// Gen is part of the match: a handle held across a delete-and-recreate
	// must not move the NEW owner of the name in anyone's listing.
	res, err := db.NewUpdate().Model((*Session)(nil)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("recording session change: %w", err)
	}
	if s.ref.Gen == "" {
		// A store used outside the repo never had a session row; nothing to
		// record is not a failure.
		return nil
	}
	// For a repo session the touch doubles as proof the session still EXISTS;
	// zero rows means deleted under this handle, and the write fails (spec §2.5e2).
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return fmt.Errorf("session %s: %w", s.ref.ID, session.ErrNotFound)
	}
	return nil
}

// appendPointIn reads where the session stands: one indexed row. No row falls
// back to the fold — answering "nothing here" would make the next append a new root.
func (s *EntryStore) appendPointIn(ctx context.Context, db bun.IDB) (session.AppendPoint, error) {
	row := new(appendPointRow)
	err := db.NewSelect().Model(row).
		Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.foldAppendPointIn(ctx, db)
	case err != nil:
		return session.AppendPoint{}, fmt.Errorf("reading the append point: %w", err)
	}
	return session.AppendPoint{Leaf: row.LeafEntryID, LastSeq: row.LastSeq}, nil
}

// foldAppendPointIn computes the append point the long way, from the rows —
// the definition the stored point must agree with (invariant 59).
func (s *EntryStore) foldAppendPointIn(ctx context.Context, db bun.IDB) (session.AppendPoint, error) {
	// Read with cross-model adaptation OFF: the stored tree is the same tree
	// whichever model reads it.
	bare := *s
	bare.model = ""
	entries, err := bare.loadIn(ctx, db, false, false)
	if err != nil {
		return session.AppendPoint{}, err
	}
	at := session.AppendPoint{Leaf: session.LeafOf(entries)}
	// The high-water mark is a MAX over every row, compacted ones included — a
	// folded-away entry still consumed its position.
	if err := db.NewSelect().Model((*entryRow)(nil)).
		ColumnExpr("COALESCE(MAX(seq), 0)").
		Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Scan(ctx, &at.LastSeq); err != nil {
		return session.AppendPoint{}, fmt.Errorf("reading the last sequence number: %w", err)
	}
	return at, nil
}

// refreshAppendPointIn refolds the session and stores where it now stands, on
// the handle that moved it.
func (s *EntryStore) refreshAppendPointIn(ctx context.Context, db bun.IDB) error {
	at, err := s.foldAppendPointIn(ctx, db)
	if err != nil {
		return err
	}
	return writeAppendPoint(ctx, db, s.ref, at)
}

// writeAppendPoint records where a session stands. Callers pass the handle that
// carried the change, so the point and the rows it describes are one write.
func writeAppendPoint(ctx context.Context, db bun.IDB, ref session.Ref, at session.AppendPoint) error {
	row := &appendPointRow{
		SessionID:   ref.ID,
		Gen:         ref.Gen,
		LeafEntryID: at.Leaf,
		LastSeq:     at.LastSeq,
	}
	if _, err := db.NewInsert().Model(row).
		On("CONFLICT (session_id, gen) DO UPDATE").
		Set("leaf_entry_id = EXCLUDED.leaf_entry_id").
		Set("last_seq = EXCLUDED.last_seq").
		Exec(ctx); err != nil {
		return fmt.Errorf("recording the append point: %w", err)
	}
	return nil
}

// load reads the session's entries in append order; the RUN excludes
// compacted rows, the UI includes them.
func (s *EntryStore) load(ctx context.Context, includeCompacted bool) ([]session.Entry, error) {
	return s.loadIn(ctx, s.db, includeCompacted, false)
}

func (s *EntryStore) loadIn(ctx context.Context, db bun.IDB, includeCompacted, strict bool) ([]session.Entry, error) {
	var rows []entryRow
	q := s.scoped(db.NewSelect().Model(&rows)).
		OrderExpr("seq ASC")
	if !includeCompacted {
		q = q.Where("compacted = ?", false)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("loading entries: %w", err)
	}
	// skipped remaps a dropped id onto its own parent so the survivors close
	// the gap; a broken parent chain would truncate the branch walk there.
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

	out := make([]session.Entry, 0, len(rows))
	for i := range rows {
		var e session.Entry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			if strict {
				// A REMOVAL cannot be decided on a view with a hole in it:
				// skipping the newest row would silently take an older one.
				return nil, fmt.Errorf("entry %q cannot be decoded: %w", rows[i].EntryID, err)
			}
			// One unreadable row must not make the whole session unloadable.
			skipped[rows[i].EntryID] = rows[i].ParentID
			continue
		}
		// An item produced by another model may be one this backend rejects
		// (a reasoning block above all): adapt it, or drop it.
		if e.Kind == session.EntryKindItem && s.model != "" &&
			rows[i].SourceModel != "" && rows[i].SourceModel != s.model {
			adapted := adaptForeignItemJSON(e.Item)
			if adapted == nil {
				skipped[e.ID] = e.ParentID
				continue
			}
			e.Item = adapted
		}
		e.ParentID = resolve(e.ParentID)
		out = append(out, e)
	}
	return out, nil
}

// Entries implements session.Storage.
func (s *EntryStore) Entries(ctx context.Context, cur session.Cursor) ([]session.Entry, error) {
	entries, err := s.load(ctx, false)
	if err != nil {
		return nil, err
	}
	return session.PageEntries(entries, cur), nil
}

// Entry implements session.Storage. Only "no such entry" is absence; a
// failure to look is an error (spec §2.5e2, "absence").
func (s *EntryStore) Entry(ctx context.Context, id string) (*session.Entry, error) {
	row := new(entryRow)
	err := s.scoped(s.db.NewSelect().Model(row)).
		Where("entry_id = ?", id).
		Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("loading entry %q: %w", id, err)
	}
	var e session.Entry
	if err := json.Unmarshal([]byte(row.Entry), &e); err != nil {
		return nil, fmt.Errorf("decoding entry %q: %w", id, err)
	}
	return &e, nil
}

// Metadata implements session.Storage, merging the session row (spec §2.5e2,
// "the change record").
func (s *EntryStore) Metadata(ctx context.Context) (session.Metadata, error) {
	n, err := s.scoped(s.db.NewSelect().Model((*entryRow)(nil))).Count(ctx)
	if err != nil {
		return session.Metadata{}, err
	}
	md := session.Metadata{ID: s.ref.ID, EntryCount: n}
	var row Session
	err = s.db.NewSelect().Model(&row).
		Where("id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Limit(1).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A store used outside the repo (or a handle to a deleted session):
		// the entries are all there is to report.
		return md, nil
	case err != nil:
		return md, err
	}
	md.Title, md.Hidden = row.Name, row.Hidden
	md.CreatedAt, md.UpdatedAt = row.CreatedAt, row.UpdatedAt
	return md, nil
}

// Clear implements session.Storage. Clearing is a change like any other,
// so it moves the session in a listing.
func (s *EntryStore) Clear(ctx context.Context) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.lockSessionIn(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*entryRow)(nil)).
			Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Exec(ctx); err != nil {
			return err
		}
		// Nothing left to stand on: back where an empty session starts,
		// numbering included (SeqFor's clock keeps the next one past them).
		if err := writeAppendPoint(ctx, tx, s.ref, session.AppendPoint{}); err != nil {
			return err
		}
		return s.touchSessionIn(ctx, tx)
	})
}

var _ session.Storage = (*EntryStore)(nil)

// EntryView is the REST shape of one entry: the stored entry plus the row id
// the cursor pages on. Nothing is re-derived.
type EntryView struct {
	ID       string `json:"id"`
	EntryID  string `json:"entry_id"`
	ParentID string `json:"parent_id,omitempty"`
	Kind     string `json:"kind"`
	RunID    string `json:"run_id,omitempty"`
	// Role is who produced the entry, from its recorded provenance rather than
	// re-parsed from the item.
	Role string `json:"role"`
	// Content is the readable text, for a renderer that wants one string.
	Content     string               `json:"content"`
	Display     *agents.ItemDisplay  `json:"display,omitempty"`
	Usage       *agents.RequestUsage `json:"usage,omitempty"`
	Diagnostics []agents.Diagnostic  `json:"diagnostics,omitempty"`
	Compacted   bool                 `json:"compacted,omitempty"`
	// OnPath reports whether this entry is on the session's ACTIVE branch; an
	// off-path entry is an abandoned attempt.
	OnPath bool `json:"on_path"`
	// Attachments are the entry's image attachments, resolved from the
	// sentinel refs its item carries; URL is filled by the handler.
	Attachments []EntryAttachment `json:"attachments,omitempty"`
	// Compaction is present on a checkpoint: what the pass folded away, so a
	// reader can collapse those entries under it and offer them back.
	Compaction *CompactionInfo `json:"compaction,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// EntryAttachment is one image attachment on an entry. Key is internal — the
// handler turns it into URL against the current public base.
type EntryAttachment struct {
	ID  string `json:"id"`
	Key string `json:"-"`
	URL string `json:"url"`
}

// entryAttachmentIDs pulls the attachment sentinel ids out of an item's wire
// JSON; nil for the vast majority of items, rejected by a byte scan first.
func entryAttachmentIDs(item json.RawMessage) []string {
	if !bytes.Contains(item, []byte(AttachmentScheme)) {
		return nil
	}
	var probe struct {
		Content []struct {
			ImageURL string `json:"image_url"`
		} `json:"content"`
	}
	if json.Unmarshal(item, &probe) != nil {
		return nil
	}
	var ids []string
	for _, p := range probe.Content {
		if id := AttachmentSentinelID(p.ImageURL); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// attachEntryAttachments fills views' Attachments from the sentinel ids their
// items carry, with one batch read; a deleted row simply drops off the view.
func (s *EntryStore) attachEntryAttachments(ctx context.Context, views []EntryView, perView [][]string) {
	var all []string
	for _, ids := range perView {
		all = append(all, ids...)
	}
	if len(all) == 0 {
		return
	}
	var rows []Attachment
	if err := s.db.NewSelect().Model(&rows).Where("id IN (?)", bun.List(all)).Scan(ctx); err != nil {
		return // a panel nicety, never worth failing the page
	}
	byID := make(map[string]Attachment, len(rows))
	for _, a := range rows {
		byID[a.ID] = a
	}
	for i, ids := range perView {
		for _, id := range ids {
			if a, ok := byID[id]; ok {
				views[i].Attachments = append(views[i].Attachments, EntryAttachment{ID: a.ID, Key: a.Key})
			}
		}
	}
}

// CompactionInfo is a checkpoint's payload minus the retained tail, which is
// already in the session as ordinary entries.
type CompactionInfo struct {
	// ExcludedIDs are the entries this checkpoint folded away — still in the
	// session, marked compacted, so a reader can collapse them under it.
	ExcludedIDs  []string `json:"excluded_ids,omitempty"`
	TokensBefore int      `json:"tokens_before,omitempty"`
	TokensAfter  int      `json:"tokens_after,omitempty"`
}

// GetEntries returns a page of a session's entries, oldest first. With a
// limit it returns the NEWEST that many and the caller pages backwards with
// the smallest id it received. Update entries are folded into their targets
// here, over the whole session before the cursor applies — so every call reads every row.
func (s *EntryStore) GetEntries(ctx context.Context, ref session.Ref, beforeID string, limit int) ([]EntryView, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		OrderExpr("seq ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("getting entries: %w", err)
	}

	entries := make([]session.Entry, 0, len(rows))
	meta := make(map[string]entryRow, len(rows))
	for i := range rows {
		var e session.Entry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			continue
		}
		meta[e.ID] = rows[i]
		entries = append(entries, e)
	}

	onPath := activeBranch(entries)
	folded := session.FoldUpdates(entries)
	views := make([]EntryView, 0, len(folded))
	attIDs := make([][]string, 0, len(folded))
	for _, e := range folded {
		row := meta[e.ID]
		attIDs = append(attIDs, entryAttachmentIDs(e.Item))
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
	s.attachEntryAttachments(ctx, views, attIDs)

	// The cursor applies to the folded list (raw row ids would give short
	// pages where an update was folded); the cut is at that row's seq.
	if beforeID != "" {
		var beforeSeq int64 = -1
		for i := range rows {
			if rows[i].ID == beforeID {
				beforeSeq = rows[i].Seq
				break
			}
		}
		cut := len(views)
		for i, v := range views {
			if beforeSeq >= 0 && meta[v.EntryID].Seq >= beforeSeq {
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

// RunQuestion is one run of a session and what it was asked (the user entry
// it started from, or for a regenerate the message it answered again), and
// whether its entries are on the active branch — the trace panel's card label.
type RunQuestion struct {
	RunID    string `json:"run_id"`
	Question string `json:"question"`
	OnPath   bool   `json:"on_path"`
}

// RunQuestions lists every run that left entries on the session's current
// generation, oldest first (a full GetEntries read).
func (s *EntryStore) RunQuestions(ctx context.Context, ref session.Ref) ([]RunQuestion, error) {
	views, err := s.GetEntries(ctx, ref, "", 0)
	if err != nil {
		return nil, err
	}
	var out []RunQuestion
	at := make(map[string]int)
	lastUser := ""
	for _, v := range views {
		if v.Role == "user" && v.Content != "" {
			lastUser = v.Content
		}
		if v.RunID == "" {
			continue
		}
		i, seen := at[v.RunID]
		if !seen {
			i = len(out)
			at[v.RunID] = i
			out = append(out, RunQuestion{RunID: v.RunID, Question: lastUser, OnPath: true})
		}
		if !v.OnPath {
			out[i].OnPath = false
		}
	}
	return out, nil
}

// roleOf maps an entry's provenance to the role a renderer groups by.
func roleOf(e session.Entry) string {
	switch e.Source.Type {
	case agents.SourceUser:
		return "user"
	case agents.SourceTool:
		return "tool"
	case agents.SourceHandoff:
		return "handoff"
	case agents.SourceCompaction:
		return "compaction"
	case agents.SourceErrorHandler, agents.SourceGuardrail, agents.SourceHost:
		return "system"
	case agents.SourceModel:
		// The zero source is the model's own output: savePartialTurn puts a
		// failed run's text on ANNOTATION entries, which must not read as "system".
		return "assistant"
	}
	if e.Kind == session.EntryKindAnnotation {
		return "system"
	}
	return "assistant"
}

// contentOf returns the entry's readable text: the display's if the runner
// produced one, otherwise the entry's own.
func contentOf(e session.Entry) string {
	if e.Display != nil && e.Display.Text != "" {
		return e.Display.Text
	}
	// A checkpoint written by a compactor that set no display still has
	// something to show — the summary standing in for what it folded.
	if e.Kind == session.EntryKindCompaction {
		if p, err := e.CompactionPayload(); err == nil {
			return p.Summary
		}
		return ""
	}
	if e.Kind != session.EntryKindItem || len(e.Item) == 0 {
		return ""
	}
	return itemTextJSON(e.Item)
}

// compactionInfoOf reports what a checkpoint folded away, or nil for any
// other entry; the kept entries are not part of it.
func compactionInfoOf(e session.Entry) *CompactionInfo {
	if e.Kind != session.EntryKindCompaction {
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
// continues from there. It appends a leaf entry rather than deleting anything.
func (s *EntryStore) Branch(ctx context.Context, ref session.Ref, entryID string) error {
	return session.NewSession(s.forRef(ref)).Branch(ctx, entryID)
}

// Leaf returns the session's active branch tip.
func (s *EntryStore) Leaf(ctx context.Context, ref session.Ref) (string, error) {
	at, err := s.forRef(ref).appendPointIn(ctx, s.db)
	if err != nil {
		return "", err
	}
	return at.Leaf, nil
}

// activeBranch returns the ids on the current branch as a set (membership is
// all the views below ask).
func activeBranch(entries []session.Entry) map[string]bool {
	byID := make(map[string]session.Entry, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}
	on := make(map[string]bool, len(entries))
	for id := session.LeafOf(entries); id != ""; {
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
// entry holds callID. An update may land before its target; projection pairs them by call id.
func (s *EntryStore) AppendCallDisplayUpdate(ctx context.Context, ref session.Ref, callID string, display agents.ItemDisplay) error {
	e, err := session.NewCallUpdateEntry(callID, display)
	if err != nil {
		return err
	}
	return s.forRef(ref).Append(ctx, e)
}

// AppendAnnotation records something shown to people but never sent to the
// model: an error banner, a cancellation notice.
func (s *EntryStore) AppendAnnotation(ctx context.Context, ref session.Ref, runID, text string) error {
	store := s.forRef(ref)
	store.runID = runID
	return store.Append(ctx, session.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayError, Text: text},
		agents.Source{Type: agents.SourceErrorHandler},
	))
}

// AppendWorkflowStarted leaves the note of a person's or a trigger's workflow
// start on the session: an annotation (people only) the result's wake-up run pairs to.
func (s *EntryStore) AppendWorkflowStarted(ctx context.Context, ref session.Ref, ws WorkflowStarted) error {
	return s.appendHostNote(ctx, ref, DisplayWorkflowStarted, ws.Text(), ws)
}

// AppendTriggerFired leaves the note of a trigger's agent turn on the
// session, before the message the turn sends: an annotation (people only).
func (s *EntryStore) AppendTriggerFired(ctx context.Context, ref session.Ref, tf TriggerFired) error {
	return s.appendHostNote(ctx, ref, DisplayTriggerFired, tf.Text(), tf)
}

// appendHostNote writes a host annotation whose display extra is data's JSON
// object; text is the line a renderer that knows no better shows.
func (s *EntryStore) appendHostNote(ctx context.Context, ref session.Ref, kind, text string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding the %s note: %w", kind, err)
	}
	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err != nil {
		return fmt.Errorf("encoding the %s note: %w", kind, err)
	}
	return s.forRef(ref).Append(ctx, session.NewAnnotationEntry(
		agents.ItemDisplay{Kind: kind, Text: text, Extra: extra},
		agents.Source{Type: agents.SourceHost, ID: kind},
	))
}

// forkEntriesTx copies a prefix of src's entries into dst, rewriting entry
// ids to the destination's namespace and remapping parent links alongside.
func forkEntriesTx(ctx context.Context, tx bun.Tx, src, dst session.Ref, upToID string, exclusive bool) ([]string, error) {
	var rows []entryRow
	q := tx.NewSelect().Model(&rows).
		Where("session_id = ?", src.ID).Where("gen = ?", src.Gen).
		OrderExpr("seq ASC")
	if upToID != "" {
		// The boundary names a row; the prefix is everything at or before
		// its position.
		at := tx.NewSelect().Model((*entryRow)(nil)).Column("seq").Where("id = ?", upToID)
		if exclusive {
			q = q.Where("seq < (?)", at)
		} else {
			q = q.Where("seq <= (?)", at)
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
	// The fork's own numbering, from the shared allocator: the destination is a
	// new session, so its positions start where any new session's would.
	seq := session.SeqFor(session.AppendPoint{})
	for i := range rows {
		if rid := rows[i].RunID; rid != "" && !seen[rid] {
			seen[rid] = true
			runIDs = append(runIDs, rid)
		}
		newID := session.EntryIDFor(seq)
		remap[rows[i].EntryID] = newID

		var e session.Entry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			continue
		}
		e.ID = newID
		e.ParentID = remap[e.ParentID] // "" for a root maps to "" — the zero value
		e.Seq = seq
		seq++
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("fork entries encode: %w", err)
		}
		rows[i].ID = "" // minted afresh on insert
		rows[i].SessionID = dst.ID
		rows[i].Gen = dst.Gen
		rows[i].Seq = e.Seq
		rows[i].EntryID = newID
		rows[i].ParentID = e.ParentID
		rows[i].Entry = string(raw)
		rows[i].CreatedAt = now
	}
	if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return nil, fmt.Errorf("fork entries write: %w", err)
	}
	// Refold to place the copy's tip: a fork copies compacted rows too, and a
	// cut landing on one would otherwise make a folded entry the tip.
	if err := (&EntryStore{ref: dst}).refreshAppendPointIn(ctx, tx); err != nil {
		return nil, err
	}
	return runIDs, nil
}

// ForkSession atomically creates dst and copies src's entries into it in one
// transaction, so a failure never leaves an orphaned empty session behind.
func (s *EntryStore) ForkSession(ctx context.Context, dst *Session, src session.Ref, upToID string, exclusive bool) ([]string, error) {
	var runIDs []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Confirm the source still exists inside the tx, by (id, gen): an id
		// alone would match a REPLACEMENT session created since.
		exists, err := tx.NewSelect().Model((*Session)(nil)).
			Where("id = ?", src.ID).Where("gen = ?", src.Gen).Exists(ctx)
		if err != nil {
			return fmt.Errorf("fork source check: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		now := time.Now().UTC()
		dst.CreatedAt, dst.UpdatedAt = now, now
		if dst.Gen == "" {
			gen, gerr := session.NewGeneration()
			if gerr != nil {
				return gerr
			}
			dst.Gen = gen
		}
		if _, err := tx.NewInsert().Model(dst).Exec(ctx); err != nil {
			return fmt.Errorf("fork create session: %w", err)
		}
		var e error
		runIDs, e = forkEntriesTx(ctx, tx, src, session.Ref{ID: dst.ID, Gen: dst.Gen}, upToID, exclusive)
		return e
	})
	if err != nil {
		return nil, err
	}
	return runIDs, nil
}

// DeleteBySession removes every entry of a session, in every generation the
// repo made; the direct scope (empty generation) is not the repo's to remove.
func (s *EntryStore) DeleteBySession(ctx context.Context, sessionID string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*entryRow)(nil)).
			Where("session_id = ?", sessionID).Where("gen <> ?", "").Exec(ctx); err != nil {
			return err
		}
		// The append point describes those rows, so it goes with them.
		_, err := tx.NewDelete().Model((*appendPointRow)(nil)).
			Where("session_id = ?", sessionID).Where("gen <> ?", "").Exec(ctx)
		return err
	})
}
