package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// seed appends entries through the store under test, which is also the only way
// they get ids and parent links.
func seed(t *testing.T, s *EntryStore, entries ...session.Entry) {
	t.Helper()
	if err := s.Append(context.Background(), entries...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func userEntry(t *testing.T, text string) session.Entry {
	t.Helper()
	return rawEntryFrom(t, `{"role":"user","content":`+quoteJSON(text)+`}`,
		agents.Source{Type: agents.SourceUser})
}

func rawEntry(t *testing.T, raw string) session.Entry {
	t.Helper()
	return rawEntryFrom(t, raw, agents.Source{})
}

// rawEntryFrom keeps the item JSON verbatim rather than round-tripping it
// through the SDK union — which is what the store does, and what lets a shape
// the union does not model (a vLLM "text" content part) reach the reader.
func rawEntryFrom(_ *testing.T, raw string, src agents.Source) session.Entry {
	return session.Entry{
		Kind:   session.EntryKindItem,
		Source: src,
		Item:   json.RawMessage(raw),
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// markCompacted folds rows away the way the compaction adapter does: the flag
// and the append point move together (see persistCompaction), or the next
// append links to an entry the fold has just taken out of the view.
func markCompacted(t *testing.T, s *EntryStore, entryIDs ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.NewUpdate().Model((*entryRow)(nil)).
		Set("compacted = ?", true).
		Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Where("entry_id IN (?)", bun.List(entryIDs)).
		Exec(ctx); err != nil {
		t.Fatalf("mark compacted: %v", err)
	}
	if err := s.refreshAppendPointIn(ctx, s.db); err != nil {
		t.Fatalf("refresh append point: %v", err)
	}
}

// End-to-end through sqlite: annotations never replay, and switching the run
// model drops foreign reasoning items and strips foreign ids while items from
// the same model replay untouched.
func TestEntryStoreReplayPolicy(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")
	s.SetModel("model-a")

	seed(t, s,
		userEntry(t, "hi"),
		rawEntry(t, `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}`),
		rawEntry(t, `{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"hello"}],"status":"completed"}`),
		session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"},
			agents.Source{Type: agents.SourceErrorHandler},
		),
	)

	// Same model: everything except the annotation replays, ids intact.
	items, err := session.NewSession(s).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("same model: want 3 items, got %d", len(items))
	}

	// Different model: reasoning dropped, assistant id stripped.
	s.SetModel("model-b")
	items, err = session.NewSession(s).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("foreign model: want 2 items (reasoning dropped), got %d", len(items))
	}
	for _, it := range items {
		raw, err := session.MarshalInputItem(it)
		if err != nil {
			t.Fatalf("marshal replayed item: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if _, ok := m["id"]; ok {
			t.Fatalf("foreign id not stripped: %s", raw)
		}
	}
}

// What the runner recorded is what the reader gets. The messages table this
// replaced kept a column per field the UI wanted and re-derived a display at
// read time, so everything the SDK knew — provenance, usage, diagnostics — had
// nowhere to go on the way in.
func TestGetEntriesPreservesWhatTheRunnerWrote(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	call := rawEntry(t, `{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"sf\"}"}`)
	call.Display = &agents.ItemDisplay{
		Kind: agents.DisplayToolCall, CallID: "c1", ToolName: "get_weather", Arguments: `{"city":"sf"}`,
	}
	call.Usage = &agents.RequestUsage{InputTokens: 11, OutputTokens: 22}
	call.Diagnostics = []agents.Diagnostic{{Type: agents.DiagToolTimeout, Message: "slow"}}
	seed(t, s, userEntry(t, "weather?"), call)

	views, err := s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 entries, got %d", len(views))
	}
	if views[0].Role != "user" || views[0].Content != "weather?" {
		t.Fatalf("user entry projected wrong: %+v", views[0])
	}
	got := views[1]
	if got.Display == nil || got.Display.ToolName != "get_weather" || got.Display.Arguments != `{"city":"sf"}` {
		t.Fatalf("display not round-tripped: %+v", got.Display)
	}
	if got.Usage == nil || got.Usage.OutputTokens != 22 {
		t.Fatalf("usage dropped: %+v", got.Usage)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Message != "slow" {
		t.Fatalf("diagnostics dropped: %+v", got.Diagnostics)
	}
	if got.RunID != "r1" || got.EntryID == "" {
		t.Fatalf("row identity missing: %+v", got)
	}
}

// An entry the runner wrote without a display still reads back with its text —
// including from Responses-compatible backends (vLLM and friends) that type
// message parts "text" rather than "output_text".
func TestGetEntriesFallsBackToItemText(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	seed(t, s, rawEntry(t, `{"type":"message","role":"assistant","content":[{"type":"text","text":"最终回答"}],"status":"completed"}`))

	views, err := s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	if len(views) != 1 || views[0].Role != "assistant" || views[0].Content != "最终回答" {
		t.Fatalf("text-part projection failed: %+v", views)
	}
}

// A failed run's streamed text is saved as a SourceModel ANNOTATION
// (savePartialTurn); it is the model's prose and must read back as assistant,
// not fall through to the annotation → "system" default and render as a chip.
func TestPartialTextAnnotationReadsBackAsAssistant(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	seed(t, s, session.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayMessage, Text: "partial answer"},
		agents.Source{Type: agents.SourceModel}))

	views, err := s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	if len(views) != 1 || views[0].Role != "assistant" || views[0].Content != "partial answer" {
		t.Fatalf("partial-text annotation projected wrong: %+v", views)
	}
}

// A compaction checkpoint names what it folded; the projection drops those
// entries, renders the summary up front, and reads the kept turns from the
// session itself — the checkpoint carries no copy of them.
func TestCompactionCheckpointFrontsTheSummary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))

	seed(t, s, userEntry(t, "old question"))
	seed(t, s, rawEntry(t, `{"type":"function_call_output","call_id":"c1","output":"kept output"}`))
	seed(t, s, rawEntry(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}],"status":"completed"}`))
	all, err := session.NewSession(s).Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	cp, err := session.NewCompactionEntry(session.CompactionPayload{
		Summary:     "summary of older history",
		ExcludedIDs: []string{all[0].ID},
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	seed(t, s, cp)

	items, err := session.NewSession(s).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items (summary + 2 kept), got %d", len(items))
	}
	first, err := session.MarshalInputItem(items[0])
	if err != nil {
		t.Fatalf("marshal first item: %v", err)
	}
	if !strings.Contains(string(first), "summary of older history") {
		t.Fatalf("summary not first, got: %s", first)
	}
	if strings.Contains(string(first), "old question") {
		t.Fatalf("folded history re-sent: %s", first)
	}
	last, _ := session.MarshalInputItem(items[2])
	if !strings.Contains(string(last), "final") {
		t.Fatalf("kept items reordered, last item: %s", last)
	}
}

// A checkpoint has to say what it folded, or the UI cannot offer it back — the
// entries are still in the session, and nothing else names them.
func TestGetEntriesReportsWhatACheckpointFolded(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))

	seed(t, s, userEntry(t, "old question"))
	cp, err := session.NewCompactionEntry(session.CompactionPayload{
		Summary:      "summary",
		ExcludedIDs:  []string{"s1-e1"},
		TokensBefore: 12400,
		TokensAfter:  3100,
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	seed(t, s, cp)

	views, err := s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 entries, got %d", len(views))
	}
	if views[0].Compaction != nil {
		t.Errorf("a plain item reported compaction info: %+v", views[0].Compaction)
	}
	got := views[1].Compaction
	if got == nil {
		t.Fatal("checkpoint reported no compaction info")
	}
	if len(got.ExcludedIDs) != 1 || got.ExcludedIDs[0] != "s1-e1" {
		t.Errorf("excluded ids = %v, want [s1-e1]", got.ExcludedIDs)
	}
	if got.TokensBefore != 12400 || got.TokensAfter != 3100 {
		t.Errorf("token estimates = %d → %d, want 12400 → 3100", got.TokensBefore, got.TokensAfter)
	}
	// The summary is readable without decoding the payload a second time.
	if views[1].Content != "summary" {
		t.Errorf("checkpoint content = %q, want the summary", views[1].Content)
	}
}

// Paging must count what the CLIENT receives, not raw rows. Folding an update
// into its target removes a row from the page, so a cursor over raw ids returns
// short pages — and a client that stops when a page is short stops early.
func TestGetEntriesPagesOverFoldedEntries(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))
	s.SetRunID("r1")

	call := rawEntry(t, `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`)
	call.Display = &agents.ItemDisplay{Kind: agents.DisplayToolCall, CallID: "c1", ToolName: "f"}
	seed(t, s, userEntry(t, "one"), call, userEntry(t, "two"), userEntry(t, "three"))
	// Two update entries: rows in the table, never entries in a page.
	if err := s.AppendCallDisplayUpdate(ctx, session.Direct("s1"), "c1", agents.ItemDisplay{Output: "done"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.AppendCallDisplayUpdate(ctx, session.Direct("s1"), "c1", agents.ItemDisplay{Text: "finished"}); err != nil {
		t.Fatalf("update 2: %v", err)
	}

	all, err := s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 entries (2 updates folded away), got %d", len(all))
	}
	// The fold happened: the call carries what the updates said.
	if all[1].Display == nil || all[1].Display.Output != "done" || all[1].Display.Text != "finished" {
		t.Fatalf("updates not folded into the call: %+v", all[1].Display)
	}

	// A limit is a count of ENTRIES. Asking for 2 must give the newest 2.
	page, err := s.GetEntries(ctx, session.Direct("s1"), 0, 2)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("newest page wrong: got %d entries, ids %v", len(page), idsOf(page))
	}

	// Paging backwards from it reaches the beginning without skipping the call
	// the updates were folded into.
	older, err := s.GetEntries(ctx, session.Direct("s1"), page[0].ID, 2)
	if err != nil {
		t.Fatalf("get older: %v", err)
	}
	if len(older) != 2 || older[0].ID != all[0].ID || older[1].ID != all[1].ID {
		t.Fatalf("cursor page wrong: got %v, want the first two", idsOf(older))
	}
}

// Branching keeps both attempts and marks which one is current. Deleting the
// abandoned one is what a fork-a-new-session regenerate did instead, and it is
// why "show me the other answer" was not offerable.
func TestBranchMarksTheActiveAttempt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct("s1"))

	seed(t, s, userEntry(t, "question"))
	seed(t, s, rawEntryFrom(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}`, agents.Source{}))

	// Go back to the question and answer it differently. Its id is read back,
	// not constructed: entry ids are opaque, and a test that rebuilds one is a
	// test of the id format.
	stored, err := s.Entries(ctx, session.Cursor{})
	if err != nil || len(stored) != 2 {
		t.Fatalf("seeded entries: %v %v", stored, err)
	}
	if err := s.Branch(ctx, session.Direct("s1"), stored[0].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}
	seed(t, s, rawEntryFrom(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}`, agents.Source{}))

	views, gerr := s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	if gerr != nil {
		t.Fatalf("get entries: %v", gerr)
	}
	onPath := map[string]bool{}
	for _, v := range views {
		if v.Content != "" {
			onPath[v.Content] = v.OnPath
		}
	}
	if !onPath["question"] || onPath["first"] || !onPath["second"] {
		t.Fatalf("wrong active branch: %v", onPath)
	}
	// Nothing was deleted — the abandoned attempt is still offerable.
	if len(views) < 4 {
		t.Fatalf("branching lost entries: %d remain", len(views))
	}

	// The model reads only the active branch.
	items, err := session.NewSession(s).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("context items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items (question + second answer), got %d", len(items))
	}
	last, _ := session.MarshalInputItem(items[1])
	if !strings.Contains(string(last), "second") {
		t.Fatalf("the abandoned answer is still in context: %s", last)
	}

	// Switching back makes the first attempt current again, by the id the
	// listing reports for it.
	var firstID string
	for _, v := range views {
		if v.Content == "first" {
			firstID = v.EntryID
		}
	}
	if firstID == "" {
		t.Fatal("the first attempt is not in the listing")
	}
	if err := s.Branch(ctx, session.Direct("s1"), firstID); err != nil {
		t.Fatalf("branch back: %v", err)
	}
	views, _ = s.GetEntries(ctx, session.Direct("s1"), 0, 0)
	for _, v := range views {
		if v.Content == "first" && !v.OnPath {
			t.Error("switching back did not restore the first attempt")
		}
		if v.Content == "second" && v.OnPath {
			t.Error("the second attempt is still on the path after switching back")
		}
	}
}

func idsOf(views []EntryView) []int64 {
	out := make([]int64, len(views))
	for i, v := range views {
		out[i] = v.ID
	}
	return out
}

// Appending, popping and clearing move the session in the listing: the entry
// store stamps the session row's updated_at in the same transaction as the
// write (spec §2.5e2, "the change record").
func TestEntryWritesBumpSessionUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	ss := NewSessionStore(db)
	if err := ss.Create(ctx, &Session{ID: sid, Name: "n"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	ref, err := RefFor(ctx, db, sid)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	s := NewEntryStoreFor(db, ref)

	rewind := func() time.Time {
		t.Helper()
		past := time.Now().UTC().Add(-time.Hour)
		if _, err := db.NewUpdate().Model((*Session)(nil)).
			Set("updated_at = ?", past).Where("id = ?", sid).Exec(ctx); err != nil {
			t.Fatalf("rewind updated_at: %v", err)
		}
		return past
	}
	updatedAt := func() time.Time {
		t.Helper()
		sess, err := ss.Get(ctx, sid)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		return sess.UpdatedAt
	}

	past := rewind()
	seed(t, s, userEntry(t, "hello"))
	if got := updatedAt(); !got.After(past) {
		t.Errorf("append did not move updated_at: %v", got)
	}

	past = rewind()
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := updatedAt(); !got.After(past) {
		t.Errorf("clear did not move updated_at: %v", got)
	}
}

// The DB enforces provider-route prefix uniqueness, and the violation is
// classified for a 409 by UniqueViolation.
func TestProviderRoutePrefixUniqueIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewProviderRouteStore(db)
	if err := s.Create(ctx, &ProviderRoute{ID: NewID(), Prefix: "gpt", APIKey: "k"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.Create(ctx, &ProviderRoute{ID: NewID(), Prefix: "gpt", APIKey: "k2"})
	if err == nil {
		t.Fatal("duplicate prefix must violate the unique index")
	}
	if cols, ok := UniqueViolation(err); !ok || cols != "prefix" {
		t.Errorf("UniqueViolation = %q,%v want \"prefix\",true", cols, ok)
	}
}

// ForkSession is atomic: when the session insert fails, the entry copy in the
// same transaction rolls back too, so no orphan session or entries are left
// behind (the gap the old create-then-copy handler left open).
func TestForkSessionAtomicNoOrphan(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)

	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := storeFor(t, db, src.ID)
	seed(t, s, userEntry(t, "hi"))

	// Reuse an existing id so the session insert inside ForkSession fails.
	taken := &Session{ID: NewID(), Name: "taken"}
	if err := sessions.Create(ctx, taken); err != nil {
		t.Fatalf("create taken: %v", err)
	}

	if _, err := s.ForkSession(ctx, &Session{ID: taken.ID, Name: "fork"}, refOf(t, db, src.ID), 0, false); err == nil {
		t.Fatal("expected ForkSession to fail on a duplicate session id")
	}
	var copied []entryRow
	if err := db.NewSelect().Model(&copied).Where("session_id = ?", taken.ID).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(copied) != 0 {
		t.Fatalf("failed fork left %d orphan entr(ies)", len(copied))
	}
}

// The in-transaction source check: forking a source that doesn't exist (e.g.
// deleted concurrently) fails with ErrNotFound and creates no empty dst.
func TestForkSessionMissingSource(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	dst := &Session{ID: NewID(), Name: "fork"}

	s := NewEntryStoreFor(db, session.Direct("nonexistent-src"))
	if _, err := s.ForkSession(ctx, dst, refOf(t, db, "nonexistent-src"), 0, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for a missing source, got %v", err)
	}
	if _, err := NewSessionStore(db).Get(ctx, dst.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("dst session must not exist after a missing-source fork")
	}
}

// ForkSession copies the source snapshot atomically, dedupes run ids, and
// leaves the source untouched.
func TestForkEntriesCopiesSnapshot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := storeFor(t, db, src.ID)
	s.SetRunID("runA")
	seed(t, s, userEntry(t, "1"), userEntry(t, "2"))
	s.SetRunID("runB")
	seed(t, s, userEntry(t, "3"))

	dst := &Session{ID: NewID(), Name: "dst"}
	runIDs, err := s.ForkSession(ctx, dst, refOf(t, db, src.ID), 0, false)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(runIDs) != 2 {
		t.Fatalf("run ids = %v, want 2 deduped", runIDs)
	}
	copied, err := s.GetEntries(ctx, refOf(t, db, dst.ID), 0, 0)
	if err != nil {
		t.Fatalf("get dst: %v", err)
	}
	if len(copied) != 3 {
		t.Fatalf("dst copied %d rows, want 3", len(copied))
	}
	// Ids are rewritten into the destination's namespace, and parent links with
	// them — a fork pointing back at another session's entries is not a tree.
	for i, e := range copied {
		if e.EntryID == "" {
			t.Fatalf("entry %d has no id", i)
		}
		if i > 0 && e.ParentID != copied[i-1].EntryID {
			t.Fatalf("entry %d parent = %q, want %q", i, e.ParentID, copied[i-1].EntryID)
		}
	}
	if orig, err := s.GetEntries(ctx, refOf(t, db, src.ID), 0, 0); err != nil || len(orig) != 3 {
		t.Fatalf("src changed by fork: %d rows (%v)", len(orig), err)
	}
}

// the upToID boundary is honored (inclusive vs exclusive).
func TestForkEntriesUpToBoundary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := storeFor(t, db, src.ID)
	s.SetRunID("r")
	seed(t, s, userEntry(t, "1"), userEntry(t, "2"), userEntry(t, "3"))

	all, err := s.GetEntries(ctx, refOf(t, db, src.ID), 0, 0)
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	cut := all[1].ID

	inc := &Session{ID: NewID(), Name: "inc"}
	if _, err := s.ForkSession(ctx, inc, refOf(t, db, src.ID), cut, false); err != nil {
		t.Fatalf("inclusive fork: %v", err)
	}
	got, _ := s.GetEntries(ctx, refOf(t, db, inc.ID), 0, 0)
	if len(got) != 2 {
		t.Fatalf("inclusive up-to copied %d, want 2", len(got))
	}

	exc := &Session{ID: NewID(), Name: "exc"}
	if _, err := s.ForkSession(ctx, exc, refOf(t, db, src.ID), cut, true); err != nil {
		t.Fatalf("exclusive fork: %v", err)
	}
	got, _ = s.GetEntries(ctx, refOf(t, db, exc.ID), 0, 0)
	if len(got) != 1 {
		t.Fatalf("exclusive up-to copied %d, want 1", len(got))
	}
}

// refOf addresses a session the way production code does: by resolving its
// generation, never by pasting an id where a ref goes.
func refOf(t *testing.T, db *bun.DB, id string) session.Ref {
	t.Helper()
	ref, err := RefFor(context.Background(), db, id)
	if err != nil {
		// A session with no row is in the direct scope, which is where a
		// handle built from a bare id writes.
		return session.Direct(id)
	}
	return ref
}

// storeFor is how a test gets a handle to a session it created a row for:
// through the row's generation, not through the id.
func storeFor(t *testing.T, db *bun.DB, id string) *EntryStore {
	t.Helper()
	return NewEntryStoreFor(db, refOf(t, db, id))
}

// The stored append point must say exactly what folding the whole session says,
// after EVERY path that moves the tip or issues a sequence number — each of
// which maintains it inside its own transaction. A path that forgets diverges
// in silence: the next append links a new entry to a tip that is not the tip,
// and the branch walk stops there with the conversation behind it dropped.
func TestAppendPointMatchesTheFold(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	s := NewEntryStoreFor(db, session.Direct(sid))
	s.SetRunID("r1")

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"append", func(t *testing.T) {
			t.Helper()
			seed(t, s, userEntry(t, "one"), userEntry(t, "two"))
		}},
		{"append again", func(t *testing.T) {
			t.Helper()
			seed(t, s, userEntry(t, "three"), userEntry(t, "four"))
		}},
		{"annotation", func(t *testing.T) {
			t.Helper()
			if err := s.AppendAnnotation(ctx, s.ref, "r1", "boom"); err != nil {
				t.Fatalf("append annotation: %v", err)
			}
		}},
		{"call display update", func(t *testing.T) {
			t.Helper()
			if err := s.AppendCallDisplayUpdate(ctx, s.ref, "c1",
				agents.ItemDisplay{Kind: agents.DisplayToolCall, CallID: "c1"}); err != nil {
				t.Fatalf("append display update: %v", err)
			}
		}},
		{"branch back", func(t *testing.T) {
			t.Helper()
			stored, err := s.Entries(ctx, session.Cursor{})
			if err != nil {
				t.Fatalf("entries: %v", err)
			}
			if err := s.Branch(ctx, s.ref, stored[0].ID); err != nil {
				t.Fatalf("branch: %v", err)
			}
		}},
		{"append onto the branch", func(t *testing.T) {
			t.Helper()
			// Enough that the pass below folds an on-path prefix: it sizes and
			// folds the ACTIVE branch only, and the branch back moved the tip
			// past everything appended before it.
			seed(t, s, userEntry(t, "five"), userEntry(t, "six"), userEntry(t, "seven"))
		}},
		{"compaction pass", func(t *testing.T) {
			t.Helper()
			ca := NewCompactionAdapter(s, &summaryFakeModel{summary: "what came before"}, 1, 2, "", CompactionNotifier{})
			if err := ca.RunCompaction(ctx, session.CompactionArgs{Force: true}); err != nil {
				t.Fatalf("compaction: %v", err)
			}
			n, err := s.scoped(db.NewSelect().Model((*entryRow)(nil))).Where("compacted = ?", true).Count(ctx)
			if err != nil {
				t.Fatalf("count folded: %v", err)
			}
			if n == 0 {
				t.Fatal("the pass folded nothing, so this step proves nothing about a fold")
			}
		}},
		{"clear", func(t *testing.T) {
			t.Helper()
			if err := s.Clear(ctx); err != nil {
				t.Fatalf("clear: %v", err)
			}
		}},
		{"append after clear", func(t *testing.T) {
			t.Helper()
			seed(t, s, userEntry(t, "six"))
		}},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			step.run(t)
			stored, err := s.appendPointIn(ctx, db)
			if err != nil {
				t.Fatalf("read the stored append point: %v", err)
			}
			folded, err := s.foldAppendPointIn(ctx, db)
			if err != nil {
				t.Fatalf("fold the append point: %v", err)
			}
			if stored != folded {
				t.Fatalf("stored append point %+v, folded %+v", stored, folded)
			}
		})
	}
}

// A database written before the point was materialized holds entries and no
// point row. Reading that as "nothing here" would make the next append a new
// root and leave the whole conversation on an abandoned branch — no error, no
// missing table, nothing to tell anyone the history had gone.
func TestAppendPointFallsBackToTheFold(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	s := NewEntryStoreFor(db, session.Direct(sid))
	s.SetRunID("r1")
	seed(t, s, userEntry(t, "one"), userEntry(t, "two"), userEntry(t, "three"))

	// What such a database holds: the entries, without the row.
	if _, err := db.NewDelete().Model((*appendPointRow)(nil)).
		Where("session_id = ?", sid).Where("gen = ?", "").Exec(ctx); err != nil {
		t.Fatalf("drop the point row: %v", err)
	}

	seed(t, s, userEntry(t, "four"))
	entries, err := s.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if n := len(session.PathToLeaf(entries, session.LeafOf(entries))); n != 4 {
		t.Fatalf("the branch walks %d of 4 entries — the append started a new root", n)
	}
	// That append also wrote the row, so the fold is paid once per session.
	exists, err := db.NewSelect().Model((*appendPointRow)(nil)).
		Where("session_id = ?", sid).Where("gen = ?", "").Exists(ctx)
	if err != nil {
		t.Fatalf("look for the point row: %v", err)
	}
	if !exists {
		t.Fatal("the append left no point row, so every later append folds again")
	}
}

// A fork is a tree of its own — its own ids, its own numbering — so it stands
// where its own entries put it. Inheriting the source's point would link the
// copy's next entry to an id that only exists in the session it was forked
// from.
func TestForkMaterializesTheDestinationAppendPoint(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := storeFor(t, db, src.ID)
	s.SetRunID("r1")
	seed(t, s, userEntry(t, "one"), userEntry(t, "two"))

	dst := &Session{ID: NewID(), Name: "dst"}
	if _, err := s.ForkSession(ctx, dst, refOf(t, db, src.ID), 0, false); err != nil {
		t.Fatalf("fork: %v", err)
	}
	forked := storeFor(t, db, dst.ID)

	stored, err := forked.appendPointIn(ctx, db)
	if err != nil {
		t.Fatalf("read the stored append point: %v", err)
	}
	folded, err := forked.foldAppendPointIn(ctx, db)
	if err != nil {
		t.Fatalf("fold the append point: %v", err)
	}
	if stored != folded {
		t.Fatalf("stored append point %+v, folded %+v", stored, folded)
	}

	// And what is appended next continues the copy's own branch.
	forked.SetRunID("r2")
	seed(t, forked, userEntry(t, "three"))
	entries, err := forked.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if n := len(session.PathToLeaf(entries, session.LeafOf(entries))); n != 3 {
		t.Fatalf("the fork's branch walks %d of 3 entries — the append did not continue it", n)
	}
}

// A fork copies compacted rows too, so the destination's UI can still show what
// was folded. Cutting ON one of them — regenerating from a message compaction
// has since folded away — must not make it the destination's tip: the fork's
// first entry would hang off an entry no view contains, putting folded messages
// back on the branch with no checkpoint to explain them.
func TestForkCutOnAFoldedEntry(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := storeFor(t, db, src.ID)
	s.SetRunID("r1")
	seed(t, s, userEntry(t, "one"), userEntry(t, "two"), userEntry(t, "three"))

	stored, err := s.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	markCompacted(t, s, stored[0].ID, stored[1].ID)
	rows := loadRows(t, db, src.ID)

	dst := &Session{ID: NewID(), Name: "dst"}
	if _, err := s.ForkSession(ctx, dst, refOf(t, db, src.ID), rows[1].ID, false); err != nil {
		t.Fatalf("fork: %v", err)
	}
	forked := storeFor(t, db, dst.ID)

	got, err := forked.appendPointIn(ctx, db)
	if err != nil {
		t.Fatalf("read the stored append point: %v", err)
	}
	want, err := forked.foldAppendPointIn(ctx, db)
	if err != nil {
		t.Fatalf("fold the append point: %v", err)
	}
	if got != want {
		t.Fatalf("stored append point %+v, folded %+v", got, want)
	}

	// Everything the fork copied was folded away, so its first entry is a root
	// and the folded copies stay off the branch.
	forked.SetRunID("r2")
	seed(t, forked, userEntry(t, "regenerated"))
	view, err := forked.GetEntries(ctx, refOf(t, db, dst.ID), 0, 10)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	for _, e := range view {
		if e.Compacted && e.OnPath {
			t.Fatalf("entry %d is folded away yet shown on the branch", e.ID)
		}
	}
}

// The append point is a property of the stored tree, not of the model reading
// it. An entry this run's backend would reject is dropped from what the MODEL
// sees, and the next entry still links behind it — otherwise the shape of the
// branch would depend on which model happened to append to it.
func TestAppendPointIgnoresTheRunModel(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStoreFor(db, session.Direct(NewID()))
	s.SetRunID("r1")
	s.SetModel("model-a")
	seed(t, s,
		userEntry(t, "hi"),
		rawEntry(t, `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}`),
	)
	stored, err := s.Entries(ctx, session.Cursor{})
	if err != nil || len(stored) != 2 {
		t.Fatalf("seeded entries: %v %v", stored, err)
	}
	reasoning := stored[1]

	// A run on another model: the reasoning item is not replayed to it...
	s.SetModel("model-b")
	items, err := session.NewSession(s).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("context items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("replayed %d items to a foreign model, want 1 (the reasoning item is not replayable)", len(items))
	}
	// ...but it is still where the session stands. Read from the rows, because
	// the same adaptation that drops the entry from the view closes the gap in
	// the parent links it leaves behind — that is a repair of the VIEW, and the
	// tree underneath must be the one every model shares.
	seed(t, s, userEntry(t, "and then?"))
	rows := loadRows(t, db, s.ref.ID)
	if got := rows[len(rows)-1].ParentID; got != reasoning.ID {
		t.Fatalf("the new entry hangs off %q, want the tip %q — a foreign tip must not fork the branch", got, reasoning.ID)
	}
}

// RunHasItems answers for ONE generation of a session id. A session deleted and
// recreated under the same name is a different session, and a stray "yes" from
// the dead one makes the caller skip a record it is the only writer of.
func TestRunHasItemsIsScopedToTheGeneration(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	sid := NewID()
	if err := sessions.Create(ctx, &Session{ID: sid, Name: "first"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	dead := storeFor(t, db, sid)
	dead.SetRunID("r1")
	seed(t, dead, userEntry(t, "hi"))

	has, err := dead.RunHasItems(ctx, "r1")
	if err != nil {
		t.Fatalf("run has items: %v", err)
	}
	if !has {
		t.Fatal("the run's own item entry was not found")
	}
	// An annotation is not a replayable item, so a run that only wrote one has
	// nothing the SDK would have persisted.
	if err := dead.AppendAnnotation(ctx, dead.ref, "r2", "boom"); err != nil {
		t.Fatalf("append annotation: %v", err)
	}
	has, err = dead.RunHasItems(ctx, "r2")
	if err != nil {
		t.Fatalf("run has items: %v", err)
	}
	if has {
		t.Fatal("an annotation counted as a persisted item")
	}

	// The same id, a new generation: the old generation's rows are not this
	// session's.
	fresh := NewEntryStoreFor(db, session.Ref{ID: sid, Gen: NewID()})
	has, err = fresh.RunHasItems(ctx, "r1")
	if err != nil {
		t.Fatalf("run has items: %v", err)
	}
	if has {
		t.Fatal("a dead generation's entries answered for the session that replaced it")
	}
}
