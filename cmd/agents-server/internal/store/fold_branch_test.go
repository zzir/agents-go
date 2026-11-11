package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// contextTexts is the model's view of s as text, one string per input item.
func contextTexts(t *testing.T, s *EntryStore) []string {
	t.Helper()
	items, err := session.NewSession(s).ContextItems(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatalf("context items: %v", err)
	}
	out := make([]string, len(items))
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encoding item %d: %v", i, err)
		}
		out[i] = session.RenderItem(raw)
	}
	return out
}

// foldedSession seeds five messages and runs one pass over them: one, two and
// three fold under a checkpoint, four and five are the kept window.
func foldedSession(t *testing.T) (*EntryStore, []session.Entry, *summaryFakeModel) {
	t.Helper()
	ctx := context.Background()
	s := NewEntryStoreFor(newTestDB(t), session.Direct(NewID()))
	seed(t, s,
		userEntry(t, "one "+strings.Repeat("x", 4000)),
		assistantEntry(t, "two"),
		userEntry(t, "three"),
		assistantEntry(t, "four"),
		userEntry(t, "five"),
	)
	seeded, err := s.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	model := &summaryFakeModel{summary: "first-summary"}
	if err := NewCompactionAdapter(s, model, 1, 2, "", CompactionNotifier{}).
		RunCompaction(ctx, session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if got, want := contextTexts(t, s), []string{session.SummaryMarker + "\n\nfirst-summary", "four", "five"}; !slices.Equal(got, want) {
		t.Fatalf("the view after the pass = %q, want %q", got, want)
	}
	return s, seeded, model
}

// firstWords cuts each text at its first space, so a padded message compares
// by its name.
func firstWords(texts []string) []string {
	out := make([]string, len(texts))
	for i, text := range texts {
		out[i], _, _ = strings.Cut(text, " ")
	}
	return out
}

// Regenerating from a message the pass kept moves the tip to before the
// checkpoint: that branch carries no checkpoint, so nothing on it is folded.
func TestBranchBeforeACheckpointReadsWhatItFolded(t *testing.T) {
	ctx := context.Background()
	s, seeded, _ := foldedSession(t)
	folded, err := s.ContextReport(ctx)
	if err != nil {
		t.Fatalf("context report: %v", err)
	}

	if err := s.Branch(ctx, seeded[3].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}
	seed(t, s, userEntry(t, "regenerated"))

	want := []string{"one", "two", "three", "four", "regenerated"}
	if got := firstWords(contextTexts(t, s)); !slices.Equal(got, want) {
		t.Fatalf("the view on the branch = %q, want %q", got, want)
	}
	branched, err := s.ContextReport(ctx)
	if err != nil {
		t.Fatalf("context report: %v", err)
	}
	if branched.ConversationTokens < folded.ConversationTokens+900 {
		t.Fatalf("the report sizes %d tokens on the branch, %d under the fold: the unfolded message is missing",
			branched.ConversationTokens, folded.ConversationTokens)
	}
}

// A branch from an entry the pass folded reads up to that entry.
func TestBranchToAFoldedEntryReadsUpToIt(t *testing.T) {
	ctx := context.Background()
	s, seeded, _ := foldedSession(t)

	if err := s.Branch(ctx, seeded[1].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}
	seed(t, s, userEntry(t, "again"))

	want := []string{"one", "two", "again"}
	if got := firstWords(contextTexts(t, s)); !slices.Equal(got, want) {
		t.Fatalf("the view on the branch = %q, want %q", got, want)
	}
}

// A pass on a branch that left its checkpoint behind folds what that branch
// reads, and names it: the summary covers the entries the earlier pass took.
func TestAPassOnABranchFoldsWhatThatBranchReads(t *testing.T) {
	ctx := context.Background()
	s, seeded, _ := foldedSession(t)
	if err := s.Branch(ctx, seeded[3].ID); err != nil {
		t.Fatalf("branch: %v", err)
	}
	seed(t, s, userEntry(t, "regenerated"), assistantEntry(t, "answer"))

	model := &summaryFakeModel{summary: "second"}
	if err := NewCompactionAdapter(s, model, 1, 2, "", CompactionNotifier{}).
		RunCompaction(ctx, session.CompactionArgs{}); err != nil {
		t.Fatalf("RunCompaction: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("summary model calls = %d, want 1", model.calls)
	}
	if tr := summaryTranscript(t, model.inputs[0]); !strings.Contains(tr, "one x") || !strings.Contains(tr, "four") {
		t.Fatalf("the summary request should carry the branch's whole prefix: %.200s", tr)
	}
	want := []string{session.SummaryMarker + "\n\nsecond", "regenerated", "answer"}
	if got := contextTexts(t, s); !slices.Equal(got, want) {
		t.Fatalf("the view after the branch's pass = %q, want %q", got, want)
	}
}

// A fork renames every entry, so the checkpoint it copies names the copies:
// the fold arrives with it.
func TestForkCarriesAFoldWithItsCheckpoint(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := storeFor(t, db, src.ID)
	seed(t, s, userEntry(t, "one"), assistantEntry(t, "two"), userEntry(t, "three"))
	seeded, err := s.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	foldAway(t, s, seeded[0].ID, seeded[1].ID)

	dst := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "dst"}
	if _, err := s.ForkSession(ctx, dst, "", false); err != nil {
		t.Fatalf("fork: %v", err)
	}
	want := []string{"folded", "three"}
	if got := contextTexts(t, storeFor(t, db, dst.ID)); !slices.Equal(got, want) {
		t.Fatalf("the fork's view = %q, want %q", got, want)
	}
}
