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
	// Gen is the session generation these entries belong to; see
	// agents.SessionRef. Empty is the direct scope, not a wildcard.
	Gen string `bun:"gen,notnull" json:"-"`
	// Seq is the entry's cursor position, allocated by agents.PrepareAppend.
	// Not the row id: that one is unique per table and assigned on insert,
	// while Seq is the session-local position a Cursor pages on.
	Seq      int64  `bun:"seq,notnull" json:"-"`
	EntryID  string `bun:"entry_id,notnull"    json:"entry_id"`
	ParentID string `bun:"parent_id"           json:"parent_id,omitempty"`
	Kind     string `bun:"kind,notnull"        json:"kind"`
	RunID    string `bun:"run_id"              json:"run_id,omitempty"`
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
	db    *bun.DB
	ref   agents.SessionRef
	runID string
	// model is what this run targets. Entries produced by a different model are
	// adapted on the way out; see load.
	model string
}

// NewEntryStore returns storage for one session.
func NewEntryStore(db *bun.DB, sessionID string) *EntryStore {
	return &EntryStore{db: db, ref: agents.Direct(sessionID)}
}

// NewEntryStoreFor returns storage addressed by ref. A repo builds one from the
// session row it has already read, so the generation is never resolved a second
// time — between two lookups an id can be deleted and recreated, and the handle
// would then be bound to the replacement.
func NewEntryStoreFor(db *bun.DB, ref agents.SessionRef) *EntryStore {
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
func (s *EntryStore) RefFor(ctx context.Context, sessionID string) (agents.SessionRef, error) {
	return RefFor(ctx, s.db, sessionID)
}

// forRef returns a handle for another session, carrying this one's run and
// model. It takes a ref, so there is no id-shaped hole for the generation to
// fall through: four methods used to build one of these by hand and three of
// them lost it.
func (s *EntryStore) forRef(ref agents.SessionRef) *EntryStore {
	return &EntryStore{db: s.db, ref: ref, runID: s.runID, model: s.model}
}

// Ref reports what this handle addresses.
func (s *EntryStore) Ref() agents.SessionRef { return s.ref }

// scoped narrows a query to this session. Every read and write of entry rows
// goes through it: that is what makes the generation part of the address rather
// than a field a code path can forget to carry.
func (s *EntryStore) scoped(q *bun.SelectQuery) *bun.SelectQuery {
	return q.Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen)
}

// PlanUnlockedKind marks the annotation entry recording that a plan-mode
// run's approved submit_plan actually EXECUTED — the durable "plan phase is
// over" mark a resume consults. Neither the approval ledger (an approved call
// can still fail argument validation and never execute) nor the tool's output
// text (rewritable by a guardrail) can stand in for it. The kind is unknown
// to the timeline renderer, so the entry never displays; annotations never
// reach the model.
const PlanUnlockedKind = "plan_unlocked"

// RunHasAnnotation reports whether the run persisted an annotation entry with
// the given display kind.
func (s *EntryStore) RunHasAnnotation(ctx context.Context, runID, kind string) (bool, error) {
	var rows []entryRow
	if err := s.scoped(s.db.NewSelect().Model(&rows)).
		Where("run_id = ?", runID).
		Scan(ctx); err != nil {
		return false, fmt.Errorf("scanning run %s entries: %w", runID, err)
	}
	for i := range rows {
		var e agents.SessionEntry
		if json.Unmarshal([]byte(rows[i].Entry), &e) != nil {
			continue
		}
		if e.Kind == agents.EntryKindAnnotation && e.Display != nil && e.Display.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}

// RefFor resolves the generation currently answering to a session id.
//
// Only "there is no such session" is absence; a cancelled context or an
// unreachable database is a failure to look, and reading the second as the
// first silently moves a handle into the direct scope.
func RefFor(ctx context.Context, db bun.IDB, sessionID string) (agents.SessionRef, error) {
	var row Session
	err := db.NewSelect().Model(&row).Column("gen").
		Where("id = ?", sessionID).Limit(1).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return agents.SessionRef{}, fmt.Errorf("session %s: %w", sessionID, ErrNotFound)
	case err != nil:
		return agents.SessionRef{}, fmt.Errorf("resolving session %s: %w", sessionID, err)
	}
	return agents.SessionRef{ID: sessionID, Gen: row.Gen}, nil
}

// SetRunID stamps subsequent writes with the run that produced them, so the UI
// can group a transcript by turn and a reaper can find one run's rows.
func (s *EntryStore) SetRunID(runID string) { s.runID = runID }

// SetModel records the model this run targets, so history produced by another
// one is adapted rather than replayed verbatim into a backend that rejects it.
func (s *EntryStore) SetModel(model string) { s.model = model }

// Append implements agents.SessionStorage.
//
// The append point is read inside the same transaction as the insert: reading
// the tip and writing against it are one step (spec §2.5e2), or two concurrent
// appends both read the old tip and silently fork the branch. The pool is
// capped at one connection (see Open), so the transaction owns the database
// for its whole extent and a competing write cannot interleave.
func (s *EntryStore) Append(ctx context.Context, entries ...agents.SessionEntry) error {
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
func (s *EntryStore) appendTo(ctx context.Context, db bun.IDB, entries ...agents.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	at, err := s.appendPointIn(ctx, db)
	if err != nil {
		return err
	}
	prepared := agents.PrepareAppend(entries, at)

	rows := make([]entryRow, 0, len(prepared))
	for i := range prepared {
		raw, err := json.Marshal(prepared[i])
		if err != nil {
			return fmt.Errorf("encoding entry %q: %w", prepared[i].ID, err)
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
			CreatedAt:   prepared[i].CreatedAt,
		})
	}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("appending %d entries: %w", len(rows), err)
	}
	return s.touchSessionIn(ctx, db)
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
		return fmt.Errorf("session %s: %w", s.ref.ID, agents.ErrSessionNotFound)
	}
	return nil
}

// appendPointIn reads where the session stands: the branch tip, and the highest
// sequence number it has issued.
//
// The high-water mark is a MAX over every row, compacted ones included — a
// folded-away entry still consumed its position, and a number this session has
// handed out is never handed out again.
func (s *EntryStore) appendPointIn(ctx context.Context, db bun.IDB) (agents.AppendPoint, error) {
	entries, err := s.loadIn(ctx, db, false, false)
	if err != nil {
		return agents.AppendPoint{}, err
	}
	var lastSeq int64
	if err := db.NewSelect().Model((*entryRow)(nil)).
		ColumnExpr("COALESCE(MAX(seq), 0)").
		Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Scan(ctx, &lastSeq); err != nil {
		return agents.AppendPoint{}, fmt.Errorf("reading the last sequence number: %w", err)
	}
	return agents.AppendPoint{Leaf: agents.LeafOf(entries), LastSeq: lastSeq}, nil
}

// load reads the session's entries in append order. Excluding compacted rows is
// the read the RUN uses; including them is what the UI uses to show what was
// folded away.
func (s *EntryStore) load(ctx context.Context, includeCompacted bool) ([]agents.SessionEntry, error) {
	return s.loadIn(ctx, s.db, includeCompacted, false)
}

func (s *EntryStore) loadIn(ctx context.Context, db bun.IDB, includeCompacted, strict bool) ([]agents.SessionEntry, error) {
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

	out := make([]agents.SessionEntry, 0, len(rows))
	for i := range rows {
		var e agents.SessionEntry
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
//
// Only "no such entry" is absence; a cancelled context or an unreachable
// database is a failure to look, and reaches the caller as one (spec §2.5e2,
// "absence"). Folding the two into nil once made "does this entry exist"
// checks silently pass over a database that was down.
func (s *EntryStore) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
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
	var e agents.SessionEntry
	if err := json.Unmarshal([]byte(row.Entry), &e); err != nil {
		return nil, fmt.Errorf("decoding entry %q: %w", id, err)
	}
	return &e, nil
}

// Metadata implements agents.SessionStorage. It merges the session row, so a
// handle and the listing give the same answer about the same session (spec
// §2.5e2, "the change record") — this was the one backend still reporting only
// a count while its own List returned title, hidden and both timestamps.
func (s *EntryStore) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	n, err := s.scoped(s.db.NewSelect().Model((*entryRow)(nil))).Count(ctx)
	if err != nil {
		return agents.SessionMetadata{}, err
	}
	md := agents.SessionMetadata{ID: s.ref.ID, EntryCount: n}
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

// Clear implements agents.SessionStorage. Clearing is a change like any other,
// so it moves the session in a listing.
func (s *EntryStore) Clear(ctx context.Context) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*entryRow)(nil)).
			Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Exec(ctx); err != nil {
			return err
		}
		return s.touchSessionIn(ctx, tx)
	})
}

// PopEntry implements agents.EntryPopper: it removes the most recent entry,
// whatever kind it is — the selection is PlanPop's, shared by every backend.
// Rows a compaction pass soft-deleted are not present to be taken; popping the
// checkpoint that folded them brings them back instead (see pop).
func (s *EntryStore) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	return s.pop(ctx, agents.PopLast)
}

// PopItem implements agents.ItemPopper: the most recent conversation item on
// the active branch, skipping what is not one — a banner, a leaf move, an
// entry a checkpoint folded away. The selection is PlanPop's, shared by every
// backend.
func (s *EntryStore) PopItem(ctx context.Context) (*agents.SessionEntry, error) {
	return s.pop(ctx, agents.PopLastItem)
}

// pop selects the entry to remove, deletes it and applies its relinks all in
// ONE transaction. Selection and deletion are one step (spec §2.5e2): chosen
// on one view and deleted on another, a concurrent append's child ends up
// hanging off an id that is gone, and a walk meeting the missing parent reads
// the session short — losing everything BEFORE the removed entry rather than
// it. The pool's single connection makes the transaction exclusive.
//
// Popping a compaction checkpoint UNDOES its fold: the entries it excluded
// come back, so the rows the adapter marked compacted are unmarked in the same
// transaction — the checkpoint and the flags are two records of one fact, and
// removing one without the other leaves rows hidden with nothing left to
// explain why.
//
// The delete still arbitrates against writers outside this process: zero rows
// affected means the entry was already gone, this caller lost, and it retries
// against what the session holds now.
func (s *EntryStore) pop(ctx context.Context, mode agents.PopMode) (*agents.SessionEntry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var popped *agents.SessionEntry
		done := true
		err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			// Compacted rows are folded away, not present: they are not
			// something a pop of either kind reaches. Strict, because a
			// removal chosen from a view missing its newest row takes the
			// wrong entry.
			entries, err := s.loadIn(ctx, tx, false, true)
			if err != nil {
				return err
			}
			plan, ok := agents.PlanPop(entries, mode)
			if !ok {
				return nil
			}
			res, derr := tx.NewDelete().Model((*entryRow)(nil)).
				Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
				Where("entry_id IN (?)", bun.List(plan.Delete)).Exec(ctx)
			if derr != nil {
				return derr
			}
			if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
				done = false
				return nil
			}
			byID := make(map[string]agents.SessionEntry, len(entries))
			for _, e := range entries {
				byID[e.ID] = e
			}
			if err := relinkIn(ctx, tx, s.ref, plan, byID); err != nil {
				return err
			}
			if err := s.unfoldIn(ctx, tx, plan.Entry); err != nil {
				return err
			}
			if err := s.touchSessionIn(ctx, tx); err != nil {
				return err
			}
			popped = &plan.Entry
			return nil
		})
		if err != nil {
			return nil, err
		}
		if done {
			return popped, nil
		}
	}
}

// unfoldIn clears the compacted flag on everything a just-removed checkpoint
// had folded, on the transaction that removed it. Not a checkpoint, or a
// checkpoint that folded nothing: nothing to do.
func (s *EntryStore) unfoldIn(ctx context.Context, tx bun.Tx, popped agents.SessionEntry) error {
	if popped.Kind != agents.EntryKindCompaction {
		return nil
	}
	p, err := popped.CompactionPayload()
	if err != nil || len(p.ExcludedIDs) == 0 {
		// An undecodable checkpoint names nothing to bring back; the pop
		// itself already succeeded.
		return nil //nolint:nilerr // see above: nothing to unfold is not a failure
	}
	if _, err := tx.NewUpdate().Model((*entryRow)(nil)).
		Set("compacted = ?", false).
		Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Where("entry_id IN (?)", bun.List(p.ExcludedIDs)).
		Exec(ctx); err != nil {
		return fmt.Errorf("unfolding entries of popped checkpoint %q: %w", popped.ID, err)
	}
	return nil
}

// relinkIn re-points the entries a removal orphaned, on the transaction that
// carried the delete.
func relinkIn(ctx context.Context, tx bun.Tx, ref agents.SessionRef, plan agents.Removal, byID map[string]agents.SessionEntry) error {
	for id, parent := range plan.Relink {
		e, ok := byID[id]
		if !ok {
			continue
		}
		if e.Kind == agents.EntryKindLeaf {
			updated, lerr := e.WithLeafTarget(parent)
			if lerr != nil {
				continue
			}
			e = updated
		} else {
			e.ParentID = parent
		}
		raw, merr := json.Marshal(e)
		if merr != nil {
			return fmt.Errorf("encoding relinked entry %q: %w", id, merr)
		}
		if _, uerr := tx.NewUpdate().Model((*entryRow)(nil)).
			Set("parent_id = ?", e.ParentID).
			Set("entry = ?", string(raw)).
			Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
			Where("entry_id = ?", id).Exec(ctx); uerr != nil {
			return uerr
		}
	}
	return nil
}

// AllEntries returns the session's entries INCLUDING compacted ones, for a UI
// that shows what a compaction folded away.
func (s *EntryStore) AllEntries(ctx context.Context) ([]agents.SessionEntry, error) {
	return s.load(ctx, true)
}

var (
	_ agents.SessionStorage = (*EntryStore)(nil)
	_ agents.EntryPopper    = (*EntryStore)(nil)
	_ agents.ItemPopper     = (*EntryStore)(nil)
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
func (s *EntryStore) GetEntries(ctx context.Context, ref agents.SessionRef, beforeID int64, limit int) ([]EntryView, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
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
// entry. The kept entries are not part of it — a checkpoint names what it
// folded and carries no copy of anything still in the session — so the client
// timeline reads them where they are.
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
func (s *EntryStore) Branch(ctx context.Context, ref agents.SessionRef, entryID string) error {
	return agents.NewSession(s.forRef(ref)).Branch(ctx, entryID)
}

// Leaf returns the session's active branch tip.
func (s *EntryStore) Leaf(ctx context.Context, ref agents.SessionRef) (string, error) {
	at, err := s.forRef(ref).appendPointIn(ctx, s.db)
	if err != nil {
		return "", err
	}
	return at.Leaf, nil
}

// activeBranch returns the ids on the current branch: the walk from the leaf up
// through parent links to the root, as a set rather than PathToLeaf's ordered
// slice, because membership is all the views below ask.
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
func (s *EntryStore) AppendCallDisplayUpdate(ctx context.Context, ref agents.SessionRef, callID string, display agents.ItemDisplay) error {
	e, err := agents.NewCallUpdateEntry(callID, display)
	if err != nil {
		return err
	}
	return s.forRef(ref).Append(ctx, e)
}

// AppendAnnotation records something shown to people but never sent to the
// model: an error banner, a cancellation notice.
func (s *EntryStore) AppendAnnotation(ctx context.Context, ref agents.SessionRef, runID, text string) error {
	store := s.forRef(ref)
	store.runID = runID
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
func forkEntriesTx(ctx context.Context, tx bun.Tx, src, dst agents.SessionRef, upToID int64, exclusive bool) ([]string, error) {
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
	seq := agents.SeqFor(agents.AppendPoint{})
	for i := range rows {
		if rid := rows[i].RunID; rid != "" && !seen[rid] {
			seen[rid] = true
			runIDs = append(runIDs, rid)
		}
		newID := agents.EntryIDFor(seq)
		remap[rows[i].EntryID] = newID

		var e agents.SessionEntry
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
	return runIDs, nil
}

// ForkSession atomically creates dst and copies src's entries into it in one
// transaction, so a failure never leaves an orphaned empty session behind.
func (s *EntryStore) ForkSession(ctx context.Context, dst *Session, src agents.SessionRef, upToID int64, exclusive bool) ([]string, error) {
	var runIDs []string
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Confirm the source still exists inside the tx: a concurrent delete
		// between the handler's read and here would otherwise yield an empty
		// entry set and a bogus empty fork returned as success.
		// By (id, gen), like every other read of a session's rows: an id alone
		// matches a REPLACEMENT session created after this fork resolved its
		// ref, and the guard would pass while forkEntriesTx copies zero rows
		// from the generation that is gone — a bogus empty fork reported as
		// success.
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
			gen, gerr := agents.NewGeneration()
			if gerr != nil {
				return gerr
			}
			dst.Gen = gen
		}
		if _, err := tx.NewInsert().Model(dst).Exec(ctx); err != nil {
			return fmt.Errorf("fork create session: %w", err)
		}
		var e error
		runIDs, e = forkEntriesTx(ctx, tx, src, agents.SessionRef{ID: dst.ID, Gen: dst.Gen}, upToID, exclusive)
		return e
	})
	if err != nil {
		return nil, err
	}
	return runIDs, nil
}

// DeleteBySession removes every entry of a session, in every generation the
// repo made — this is the teardown, and a superseded generation's rows are
// unreachable garbage once the session row is gone.
//
// The direct scope is excluded: an empty generation belongs to a session this
// server did not create through a repo, so removing it here would destroy
// history the caller keeps somewhere else.
func (s *EntryStore) DeleteBySession(ctx context.Context, sessionID string) error {
	_, err := s.db.NewDelete().Model((*entryRow)(nil)).
		Where("session_id = ?", sessionID).Where("gen <> ?", "").Exec(ctx)
	return err
}
