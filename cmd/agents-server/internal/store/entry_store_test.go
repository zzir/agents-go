package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
)

// seed appends entries through the store under test, which is also the only way
// they get ids and parent links.
func seed(t *testing.T, s *EntryStore, entries ...agents.SessionEntry) {
	t.Helper()
	if err := s.Append(context.Background(), entries...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func userEntry(t *testing.T, text string) agents.SessionEntry {
	t.Helper()
	return rawEntryFrom(t, `{"role":"user","content":`+quoteJSON(text)+`}`,
		agents.Source{Type: agents.SourceUser})
}

func rawEntry(t *testing.T, raw string) agents.SessionEntry {
	t.Helper()
	return rawEntryFrom(t, raw, agents.Source{})
}

// rawEntryFrom keeps the item JSON verbatim rather than round-tripping it
// through the SDK union — which is what the store does, and what lets a shape
// the union does not model (a vLLM "text" content part) reach the reader.
func rawEntryFrom(_ *testing.T, raw string, src agents.Source) agents.SessionEntry {
	return agents.SessionEntry{
		Kind:   agents.EntryKindItem,
		Source: src,
		Item:   json.RawMessage(raw),
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// End-to-end through sqlite: annotations never replay, and switching the run
// model drops foreign reasoning items and strips foreign ids while items from
// the same model replay untouched.
func TestEntryStoreReplayPolicy(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStore(db, "s1")
	s.SetRunID("r1")
	s.SetModel("model-a")

	seed(t, s,
		userEntry(t, "hi"),
		rawEntry(t, `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}`),
		rawEntry(t, `{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"hello"}],"status":"completed"}`),
		agents.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"},
			agents.Source{Type: agents.SourceErrorHandler},
		),
	)

	// Same model: everything except the annotation replays, ids intact.
	items, err := agents.NewSession(s).ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("same model: want 3 items, got %d", len(items))
	}

	// Different model: reasoning dropped, assistant id stripped.
	s.SetModel("model-b")
	items, err = agents.NewSession(s).ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("foreign model: want 2 items (reasoning dropped), got %d", len(items))
	}
	for _, it := range items {
		raw, err := agents.MarshalInputItem(it)
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
	s := NewEntryStore(db, "s1")
	s.SetRunID("r1")

	call := rawEntry(t, `{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"sf\"}"}`)
	call.Display = &agents.ItemDisplay{
		Kind: agents.DisplayToolCall, CallID: "c1", ToolName: "get_weather", Arguments: `{"city":"sf"}`,
	}
	call.Usage = &agents.RequestUsage{InputTokens: 11, OutputTokens: 22}
	call.Diagnostics = []agents.Diagnostic{{Type: agents.DiagToolTimeout, Message: "slow"}}
	seed(t, s, userEntry(t, "weather?"), call)

	views, err := s.GetEntries(ctx, "s1", 0, 0)
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
	s := NewEntryStore(db, "s1")
	seed(t, s, rawEntry(t, `{"type":"message","role":"assistant","content":[{"type":"text","text":"最终回答"}],"status":"completed"}`))

	views, err := s.GetEntries(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("get entries: %v", err)
	}
	if len(views) != 1 || views[0].Role != "assistant" || views[0].Content != "最终回答" {
		t.Fatalf("text-part projection failed: %+v", views)
	}
}

// A compaction checkpoint stands in for everything before it, so the model sees
// [summary, kept…] — by construction, not because the reader hoists a row to
// the front the way the messages table had to.
func TestCompactionCheckpointFrontsTheSummary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStore(db, "s1")

	seed(t, s, userEntry(t, "old question"))
	kept := []agents.TResponseInputItem{
		mustItem(t, `{"type":"function_call_output","call_id":"c1","output":"kept output"}`),
		mustItem(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}],"status":"completed"}`),
	}
	cp, err := agents.NewCompactionEntry(agents.CompactionPayload{
		Summary:     "summary of older history",
		ExcludedIDs: []string{"s1-e1"},
	}, kept)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	seed(t, s, cp)

	items, err := agents.NewSession(s).ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items (summary + 2 retained), got %d", len(items))
	}
	first, err := agents.MarshalInputItem(items[0])
	if err != nil {
		t.Fatalf("marshal first item: %v", err)
	}
	if !strings.Contains(string(first), "summary of older history") {
		t.Fatalf("summary not first, got: %s", first)
	}
	last, _ := agents.MarshalInputItem(items[2])
	if !strings.Contains(string(last), "final") {
		t.Fatalf("retained items reordered, last item: %s", last)
	}
}

// A checkpoint has to say what it folded, or the UI cannot offer it back — the
// entries are still in the session, and nothing else names them.
func TestGetEntriesReportsWhatACheckpointFolded(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewEntryStore(db, "s1")

	seed(t, s, userEntry(t, "old question"))
	cp, err := agents.NewCompactionEntry(agents.CompactionPayload{
		Summary:      "summary",
		ExcludedIDs:  []string{"s1-e1"},
		TokensBefore: 12400,
		TokensAfter:  3100,
	}, nil)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	seed(t, s, cp)

	views, err := s.GetEntries(ctx, "s1", 0, 0)
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
	s := NewEntryStore(db, "s1")
	s.SetRunID("r1")

	call := rawEntry(t, `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`)
	call.Display = &agents.ItemDisplay{Kind: agents.DisplayToolCall, CallID: "c1", ToolName: "f"}
	seed(t, s, userEntry(t, "one"), call, userEntry(t, "two"), userEntry(t, "three"))
	// Two update entries: rows in the table, never entries in a page.
	if err := s.AppendCallDisplayUpdate(ctx, "s1", "c1", agents.ItemDisplay{Output: "done"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.AppendCallDisplayUpdate(ctx, "s1", "c1", agents.ItemDisplay{Text: "finished"}); err != nil {
		t.Fatalf("update 2: %v", err)
	}

	all, err := s.GetEntries(ctx, "s1", 0, 0)
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
	page, err := s.GetEntries(ctx, "s1", 0, 2)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("newest page wrong: got %d entries, ids %v", len(page), idsOf(page))
	}

	// Paging backwards from it reaches the beginning without skipping the call
	// the updates were folded into.
	older, err := s.GetEntries(ctx, "s1", page[0].ID, 2)
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
	s := NewEntryStore(db, "s1")

	seed(t, s, userEntry(t, "question"))
	seed(t, s, rawEntryFrom(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}`, agents.Source{}))

	// Go back to the question and answer it differently. Its id is read back,
	// not constructed: entry ids are opaque, and a test that rebuilds one is a
	// test of the id format.
	stored, err := s.Entries(ctx, agents.Cursor{})
	if err != nil || len(stored) != 2 {
		t.Fatalf("seeded entries: %v %v", stored, err)
	}
	if err := s.Branch(ctx, "s1", stored[0].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}
	seed(t, s, rawEntryFrom(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}`, agents.Source{}))

	views, gerr := s.GetEntries(ctx, "s1", 0, 0)
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
	items, err := agents.NewSession(s).ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatalf("context items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items (question + second answer), got %d", len(items))
	}
	last, _ := agents.MarshalInputItem(items[1])
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
	if err := s.Branch(ctx, "s1", firstID); err != nil {
		t.Fatalf("branch back: %v", err)
	}
	views, _ = s.GetEntries(ctx, "s1", 0, 0)
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

func mustItem(t *testing.T, raw string) agents.TResponseInputItem {
	t.Helper()
	item, err := agents.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return item
}

// PopItem is "undo the last thing the model said": (nil, nil) on an empty
// session, and never a UI-only annotation or a compacted (soft-deleted) row.
// PopEntry, which takes the most recent entry whatever it is, is held to its
// own contract by agentstest.StorageConformance.
func TestEntryStorePopItem(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	s := NewEntryStore(db, sid)

	// Empty session -> (nil, nil), not an error.
	got, err := s.PopItem(ctx)
	if err != nil || got != nil {
		t.Fatalf("empty session: got=%v err=%v, want nil,nil", got, err)
	}

	// Oldest -> newest: a real item, then a compacted item, then an annotation.
	seed(t, s, userEntry(t, "hi"), userEntry(t, "folded"))
	// By position, not by a constructed id.
	stored, serr := s.Entries(ctx, agents.Cursor{})
	if serr != nil || len(stored) != 2 {
		t.Fatalf("seeded entries: %v %v", stored, serr)
	}
	if _, err := db.NewUpdate().Model((*entryRow)(nil)).
		Set("compacted = ?", true).
		Where("session_id = ?", sid).Where("entry_id = ?", stored[1].ID).
		Exec(ctx); err != nil {
		t.Fatalf("mark compacted: %v", err)
	}
	seed(t, s, agents.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"},
		agents.Source{Type: agents.SourceErrorHandler},
	))

	// The newest row is the annotation and the one before is compacted, but
	// PopEntry must skip both and return the real item.
	got, err = s.PopItem(ctx)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if got == nil {
		t.Fatal("expected the replayable item, got nil")
	}

	// The annotation and compacted rows must still be present (not deleted).
	var remaining []entryRow
	if err := db.NewSelect().Model(&remaining).Where("session_id = ?", sid).Scan(ctx); err != nil {
		t.Fatalf("scan remaining: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining rows = %d, want 2 (annotation + compacted untouched)", len(remaining))
	}
	for _, r := range remaining {
		if r.Kind != string(agents.EntryKindAnnotation) && !r.Compacted {
			t.Errorf("PopEntry deleted the wrong row; a plain item survived: %+v", r)
		}
	}

	// No replayable items left -> (nil, nil) again even though rows exist.
	got, err = s.PopItem(ctx)
	if err != nil || got != nil {
		t.Fatalf("no replayable items: got=%v err=%v, want nil,nil", got, err)
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
	s := NewEntryStore(db, src.ID)
	seed(t, s, userEntry(t, "hi"))

	// Reuse an existing id so the session insert inside ForkSession fails.
	taken := &Session{ID: NewID(), Name: "taken"}
	if err := sessions.Create(ctx, taken); err != nil {
		t.Fatalf("create taken: %v", err)
	}

	if _, err := s.ForkSession(ctx, &Session{ID: taken.ID, Name: "fork"}, src.ID, 0, false); err == nil {
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

	s := NewEntryStore(db, "nonexistent-src")
	if _, err := s.ForkSession(ctx, dst, "nonexistent-src", 0, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for a missing source, got %v", err)
	}
	if _, err := NewSessionStore(db).Get(ctx, dst.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("dst session must not exist after a missing-source fork")
	}
}

// a row whose entry JSON can't be deserialized must survive: the delete only
// commits after a successful decode, so a decode failure rolls back.
func TestPopEntryRollsBackOnUndecodableRow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	s := NewEntryStore(db, sid)

	seed(t, s, userEntry(t, "keep me"))
	// A newer row with non-empty but undecodable entry JSON.
	bad := entryRow{
		SessionID: sid, RunID: "r", EntryID: "corrupt",
		Kind: string(agents.EntryKindItem), Entry: `{"kind":`, CreatedAt: time.Now().UTC(),
	}
	if _, err := db.NewInsert().Model(&bad).Exec(ctx); err != nil {
		t.Fatalf("insert bad: %v", err)
	}

	if _, err := s.PopEntry(ctx); err == nil {
		t.Fatal("expected an error popping an undecodable row")
	}
	// Neither row was deleted — no silent data loss.
	var remaining []entryRow
	if err := db.NewSelect().Model(&remaining).Where("session_id = ?", sid).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("rows = %d, want 2 (nothing lost on decode failure)", len(remaining))
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
	s := NewEntryStore(db, src.ID)
	s.SetRunID("runA")
	seed(t, s, userEntry(t, "1"), userEntry(t, "2"))
	s.SetRunID("runB")
	seed(t, s, userEntry(t, "3"))

	dst := &Session{ID: NewID(), Name: "dst"}
	runIDs, err := s.ForkSession(ctx, dst, src.ID, 0, false)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(runIDs) != 2 {
		t.Fatalf("run ids = %v, want 2 deduped", runIDs)
	}
	copied, err := s.GetEntries(ctx, dst.ID, 0, 0)
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
	if orig, err := s.GetEntries(ctx, src.ID, 0, 0); err != nil || len(orig) != 3 {
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
	s := NewEntryStore(db, src.ID)
	s.SetRunID("r")
	seed(t, s, userEntry(t, "1"), userEntry(t, "2"), userEntry(t, "3"))

	all, err := s.GetEntries(ctx, src.ID, 0, 0)
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	cut := all[1].ID

	inc := &Session{ID: NewID(), Name: "inc"}
	if _, err := s.ForkSession(ctx, inc, src.ID, cut, false); err != nil {
		t.Fatalf("inclusive fork: %v", err)
	}
	got, _ := s.GetEntries(ctx, inc.ID, 0, 0)
	if len(got) != 2 {
		t.Fatalf("inclusive up-to copied %d, want 2", len(got))
	}

	exc := &Session{ID: NewID(), Name: "exc"}
	if _, err := s.ForkSession(ctx, exc, src.ID, cut, true); err != nil {
		t.Fatalf("exclusive fork: %v", err)
	}
	got, _ = s.GetEntries(ctx, exc.ID, 0, 0)
	if len(got) != 1 {
		t.Fatalf("exclusive up-to copied %d, want 1", len(got))
	}
}
