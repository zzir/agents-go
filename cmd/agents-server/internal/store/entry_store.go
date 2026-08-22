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
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/session"
)

// entryRow is one session entry. The whole entry is stored as JSON, with only
// the columns the queries need lifted out, so nothing the SDK's entry model
// carries (provenance, usage, diagnostics, the parent link) is dropped.
type entryRow struct {
	bun.BaseModel `bun:"table:entries,alias:e"`

	ID        int64  `bun:"id,pk,autoincrement" json:"id"`
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	// Gen is the session generation these entries belong to; see
	// session.Ref. Empty is the direct scope, not a wildcard.
	Gen string `bun:"gen,notnull" json:"-"`
	// Seq is the entry's cursor position, allocated by session.PrepareAppend.
	// Not the row id: that one is unique per table and assigned on insert,
	// while Seq is the session-local position a Cursor pages on.
	Seq      int64  `bun:"seq,notnull" json:"-"`
	EntryID  string `bun:"entry_id,notnull"    json:"entry_id"`
	ParentID string `bun:"parent_id"           json:"parent_id,omitempty"`
	Kind     string `bun:"kind,notnull"        json:"kind"`
	RunID    string `bun:"run_id,nullzero,type:uuid" json:"run_id,omitempty"`
	// Entry is the JSON of an session.Entry.
	Entry string `bun:"entry,type:text,notnull" json:"-"`
	// SourceModel records which model produced the entry, so replaying the
	// session against a different one can adapt or drop what that one would
	// reject — reasoning items above all.
	SourceModel string `bun:"source_model" json:"-"`
	// Usage and EstTokens are lifted out of Entry so a reader can total and
	// rank a session without reading its contents — the difference between a
	// cost proportional to the session's BYTES and one proportional to its row
	// count (see ContextReport). Usage is the entry's RequestUsage as JSON,
	// empty for the entries that carry none; EstTokens is CharEstimator's size
	// for the entry as stored, which is what the compaction pass compares.
	// Both are written by every append, and a database from a build that had
	// neither refuses to start (verifySchema), so a reader never has to wonder
	// whether a row was measured.
	Usage     string `bun:"usage,nullzero" json:"-"`
	EstTokens int    `bun:"est_tokens"     json:"-"`
	// Compacted marks an entry the compaction pass folded away. It is a
	// soft delete: the row stays so the UI can still show what was folded.
	Compacted bool      `bun:"compacted"          json:"compacted,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

// appendPointRow is where one session stands: the branch tip, and the highest
// sequence number it holds. It is stored rather than folded out of the entries
// so an append need not read the whole session (which only grows, since
// compaction soft-deletes). Not a cache: every path that moves the tip or issues
// a sequence number writes it in the same transaction as the change, so the two
// cannot come apart. foldAppendPointIn is the definition it must agree with.
type appendPointRow struct {
	bun.BaseModel `bun:"table:append_points,alias:ap"`

	SessionID string `bun:"session_id,pk,type:uuid"`
	// Gen is the generation this point belongs to — part of the key, as it is
	// part of every other address of an entry row (see EntryStore.scoped).
	Gen string `bun:"gen,pk"`
	// LeafEntryID is the tip the next append links to; empty starts a root.
	LeafEntryID string `bun:"leaf_entry_id,notnull"`
	// LastSeq is the highest sequence number the session HOLDS, which a
	// removal lowers. session.AppendPoint would rather have the highest ever
	// issued; SeqFor is written so the weaker answer is still safe, because it
	// takes the clock over this floor.
	LastSeq int64 `bun:"last_seq,notnull"`
}

// EntryStore persists a server session's entries and serves them to the SDK. It
// implements session.Storage directly, so an entry goes in as the runner
// produced it and comes back the same, display and provenance included.
type EntryStore struct {
	db    *bun.DB
	ref   session.Ref
	runID string
	// model is what this run targets. Entries produced by a different model are
	// adapted on the way out; see load.
	model string
}

// NewEntryStoreFor returns storage addressed by ref. A repo builds one from the
// session row it already read, so the generation is never resolved a second
// time — between two lookups an id can be deleted and recreated.
func NewEntryStoreFor(db *bun.DB, ref session.Ref) *EntryStore {
	return &EntryStore{db: db, ref: ref}
}

// NewSharedEntryStore returns a handle that owns no session, for the methods
// that take a ref explicitly (GetEntries, DeleteBySession, ForkSession, and the
// per-session handles forRef builds). It addresses nothing itself.
func NewSharedEntryStore(db *bun.DB) *EntryStore {
	return &EntryStore{db: db}
}

// RefFor resolves the generation currently answering to a session id, for a
// caller that holds a shared handle rather than the database.
func (s *EntryStore) RefFor(ctx context.Context, sessionID string) (session.Ref, error) {
	return RefFor(ctx, s.db, sessionID)
}

// forRef returns a handle for another session, carrying this one's run and
// model. It takes a ref, so there is no id-shaped hole for the generation to
// fall through.
func (s *EntryStore) forRef(ref session.Ref) *EntryStore {
	return &EntryStore{db: s.db, ref: ref, runID: s.runID, model: s.model}
}

// scoped narrows a query to this session. Every read and write of entry rows
// goes through it, so the generation is part of the address, not a field a code
// path can forget to carry.
func (s *EntryStore) scoped(q *bun.SelectQuery) *bun.SelectQuery {
	return q.Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen)
}

// SessionIsPlanning reports whether the session should START its next run in
// the planning phase — which it does only when somebody ASKED for a plan and
// has not since had one approved. It is a single-row read of the materialized
// column (see Session.Planning), not a scan of the entry log.
//
// The state belongs to the SESSION, not a run: a plan approved in one turn is
// not re-asked in the next, which is what a person means by approving a plan.
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

// SetSessionPlanning writes the session's plan phase. The last write wins, and
// the approved submit_plan's unlock is one of them — persisting it is the
// precondition for a run leaving the planning phase (see armPlanUnlock).
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

// RunHasItems reports whether the run persisted any replayable item entry —
// the user's input, or an item of a turn that completed. A caller writing a
// fallback record of a run that died asks this first, so it does not duplicate
// what the SDK's per-turn persistence already saved. Scoped by generation like
// every other entry read.
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

// RefFor resolves the generation currently answering to a session id.
//
// Only "there is no such session" is absence; a cancelled context or an
// unreachable database is a failure to look, and reading the second as the
// first silently moves a handle into the direct scope.
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

// Append implements session.Storage.
//
// The append point is read inside the same transaction as the insert: reading
// the tip and writing against it are one step (spec §2.5e2), or two concurrent
// appends both read the old tip and silently fork the branch. On SQLite the
// single-connection pool serializes the two outright; on Postgres two appends
// can interleave, and the (session, gen, seq) unique index turns the loser
// into a failed write — either way a forked branch cannot land.
func (s *EntryStore) Append(ctx context.Context, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.appendTo(ctx, tx, entries...)
	})
}

// appendTo is Append against a specific handle, so a caller that must append as
// part of a larger change — compaction marking rows folded and writing its
// checkpoint — can do both in one transaction instead of leaving a window where
// the session has one without the other.
func (s *EntryStore) appendTo(ctx context.Context, db bun.IDB, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
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

// appendPointAfter reports where the session stands once prepared has been
// written: the same fold session.PrepareAppend just linked them with — a leaf
// entry moves the tip to its target, anything else becomes the tip.
//
// It is seeded with the previous point rather than read off the entries alone,
// because an append of nothing but leaf moves leaves the tip where it was, and
// says nothing about the numbers already issued.
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

// touchSessionIn records that the session changed, on the same handle as the
// change — a listing sorts by recency of change, and an append, a pop or a
// clear is a change exactly as a rename is. Zero rows affected means no
// session row (a store used outside the repo), which is nothing to record
// rather than a failure.
func (s *EntryStore) touchSessionIn(ctx context.Context, db bun.IDB) error {
	// Gen is part of the match: a handle held across a delete-and-recreate
	// writes its entries into the old generation's scope, and must not move
	// the NEW owner of the name in anyone's listing.
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
	// For a repo session the touch doubles as proof the session still EXISTS:
	// zero rows means it was deleted under this handle, and the write must
	// fail and roll back rather than mint entries no listing reaches and no
	// delete can remove (spec §2.5e2: writing and proving the destination
	// still exists are one step).
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return fmt.Errorf("session %s: %w", s.ref.ID, session.ErrNotFound)
	}
	return nil
}

// appendPointIn reads where the session stands: one indexed row, whatever the
// session's length (see appendPointRow). No row falls back to the fold, which is
// right both for a never-appended session and for one predating this table —
// answering "nothing here" would make the next append a new root and abandon the
// conversation behind it. A path that forgets to maintain the point leaves a
// STALE row, not a missing one, so the fallback cannot hide it.
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

// foldAppendPointIn computes the append point the long way, from the rows.
//
// It is the definition the stored point must agree with, and the way back to
// agreement for a change that cannot say where the tip landed without looking:
// a pop, which deletes and relinks, or a fold, which takes rows out of the view.
func (s *EntryStore) foldAppendPointIn(ctx context.Context, db bun.IDB) (session.AppendPoint, error) {
	// Read with cross-model adaptation OFF: the stored tree is the same tree
	// whichever model reads it. An entry this run's backend would reject is
	// still where the session stands, and linking the next one past it would
	// give the branch a different shape for every model that ever ran here —
	// adaptation is a view of the history, not a rewrite of it.
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

// load reads the session's entries in append order. Excluding compacted rows is
// the read the RUN uses; including them is what the UI uses to show what was
// folded away.
func (s *EntryStore) load(ctx context.Context, includeCompacted bool) ([]session.Entry, error) {
	return s.loadIn(ctx, s.db, includeCompacted, false)
}

func (s *EntryStore) loadIn(ctx context.Context, db bun.IDB, includeCompacted, strict bool) ([]session.Entry, error) {
	var rows []entryRow
	q := s.scoped(db.NewSelect().Model(&rows)).
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

	out := make([]session.Entry, 0, len(rows))
	for i := range rows {
		var e session.Entry
		if err := json.Unmarshal([]byte(rows[i].Entry), &e); err != nil {
			if strict {
				// A REMOVAL cannot be decided on a view with a hole in it: the
				// entry it would take is chosen by recency, and skipping the
				// newest row silently takes an older one instead. Reading is
				// different — see below — but nothing is deleted by a read.
				return nil, fmt.Errorf("entry %q cannot be decoded: %w", rows[i].EntryID, err)
			}
			// One unreadable row must not make the whole session unloadable:
			// the rest of the conversation is still valid, and refusing to open
			// it would lose everything to one bad record.
			skipped[rows[i].EntryID] = rows[i].ParentID
			continue
		}
		// An item produced by another model may be one this backend rejects —
		// a reasoning block above all, whose shape and ids are provider
		// specific. Adapt it, or drop it when it cannot be adapted.
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

// Entry implements session.Storage.
//
// Only "no such entry" is absence; a cancelled context or an unreachable
// database is a failure to look, and reaches the caller as one (spec §2.5e2,
// "absence"), not folded into a nil that reads as "no such entry".
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

// Metadata implements session.Storage. It merges the session row, so a handle
// and the listing give the same answer about the same session (spec §2.5e2,
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
		if _, err := tx.NewDelete().Model((*entryRow)(nil)).
			Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Exec(ctx); err != nil {
			return err
		}
		// Nothing left to stand on: the session is back where an empty one
		// starts, numbering included — the rows that held those numbers are
		// gone, and SeqFor's clock keeps the next one past them anyway.
		if err := writeAppendPoint(ctx, tx, s.ref, session.AppendPoint{}); err != nil {
			return err
		}
		return s.touchSessionIn(ctx, tx)
	})
}

var _ session.Storage = (*EntryStore)(nil)

// EntryView is the REST shape of one entry: the stored entry plus the row id the
// cursor pages on. Nothing is re-derived here — Display, Source, Usage and
// Diagnostics come from what the runner wrote.
type EntryView struct {
	ID       int64  `json:"id"`
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
// an offset would shift under a concurrent append.
//
// Update entries are folded into their targets here, not shipped to the client:
// folding is the SDK's rule and there should be one implementation. It happens
// over the whole session before the cursor is applied (an update and the entry
// it amends need not land in the same page), so this reads every row on every
// call — a known cost, paid once per page a person asks for.
func (s *EntryStore) GetEntries(ctx context.Context, ref session.Ref, beforeID int64, limit int) ([]EntryView, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		OrderExpr("id ASC").Scan(ctx); err != nil {
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

// RunQuestion is one run of a session and what it was asked: the text of the
// user entry it started from — its own, or for a regenerate the message it
// answered again — and whether its entries are on the active branch. The trace
// panel labels a run's card by it when the entry lies outside the page of
// history it has loaded, and marks a branched-away run stale.
type RunQuestion struct {
	RunID    string `json:"run_id"`
	Question string `json:"question"`
	OnPath   bool   `json:"on_path"`
}

// RunQuestions lists every run that left entries on the session's current
// generation, oldest first — the same walk over the entries the client makes
// over the page it holds, here over all of them (a GetEntries read, the cost
// the messages page pays too).
func (s *EntryStore) RunQuestions(ctx context.Context, ref session.Ref) ([]RunQuestion, error) {
	views, err := s.GetEntries(ctx, ref, 0, 0)
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
		// The zero source: the model's own output. savePartialTurn puts a failed
		// run's streamed text/reasoning on ANNOTATION entries, which must not fall
		// through to the annotation → "system" default below.
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

// compactionInfoOf reports what a checkpoint folded away, or nil for any other
// entry. The kept entries are not part of it — a checkpoint names what it
// folded and carries no copy of anything still in the session — so the client
// timeline reads them where they are.
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
// continues from there.
//
// It appends a leaf entry rather than deleting anything: the abandoned attempt
// stays recorded, which is what makes "try that again differently" reversible
// and what lets the UI offer both versions.
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

// activeBranch returns the ids on the current branch: the walk from the leaf up
// through parent links to the root, as a set rather than PathToLeaf's ordered
// slice, because membership is all the views below ask.
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

// AppendCallDisplayUpdate records an amendment to the display of whichever entry
// holds callID. It is an APPEND: an update entry may be stored before its target
// (a background task can finish before the turn that spawned it is persisted),
// and projection associates them by call id afterwards, so there is nothing to
// wait for.
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
// start on the session: an annotation (people only), with the data a
// renderer pairs the result's wake-up run to.
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

// forkEntriesTx copies a prefix of src's entries into dst. Entry ids are
// rewritten to the destination's namespace and parent links remapped alongside,
// so the fork is a self-consistent tree — copying ids verbatim would make the
// two sessions' entries indistinguishable by the id every lookup keys on.
func forkEntriesTx(ctx context.Context, tx bun.Tx, src, dst session.Ref, upToID int64, exclusive bool) ([]string, error) {
	var rows []entryRow
	q := tx.NewSelect().Model(&rows).
		Where("session_id = ?", src.ID).Where("gen = ?", src.Gen).
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
		rows[i].ID = 0
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
	// The copy is a tree of its own, so refold to place its tip: a fork copies
	// compacted rows too (the UI shows what was folded), and a cut landing on one
	// would otherwise make an already-folded entry the destination's tip.
	if err := (&EntryStore{ref: dst}).refreshAppendPointIn(ctx, tx); err != nil {
		return nil, err
	}
	return runIDs, nil
}

// ForkSession atomically creates dst and copies src's entries into it in one
// transaction, so a failure never leaves an orphaned empty session behind.
func (s *EntryStore) ForkSession(ctx context.Context, dst *Session, src session.Ref, upToID int64, exclusive bool) ([]string, error) {
	var runIDs []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Confirm the source still exists inside the tx, by (id, gen): a
		// concurrent delete would otherwise yield an empty entry set and a bogus
		// empty fork reported as success. An id alone would match a REPLACEMENT
		// session created after this fork resolved its ref.
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

// DeleteBySession removes every entry of a session, in every generation the repo
// made. The direct scope (empty generation) is excluded: it belongs to a session
// this server did not create through a repo, so removing it here would destroy
// history the caller keeps somewhere else.
func (s *EntryStore) DeleteBySession(ctx context.Context, sessionID string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*entryRow)(nil)).
			Where("session_id = ?", sessionID).Where("gen <> ?", "").Exec(ctx); err != nil {
			return err
		}
		// The append point describes those rows, so it goes with them: left
		// behind, it would point a later handle on the same generation at a tip
		// that is no longer there.
		_, err := tx.NewDelete().Model((*appendPointRow)(nil)).
			Where("session_id = ?", sessionID).Where("gen <> ?", "").Exec(ctx)
		return err
	})
}
