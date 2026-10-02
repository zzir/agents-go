package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
)

// setUpdated pins a session's updated_at, so the keyset order is exercised
// with ties as well as gaps.
func setUpdated(t *testing.T, db *bun.DB, id string, at time.Time) {
	t.Helper()
	if _, err := db.NewUpdate().Model((*Session)(nil)).Set("updated_at = ?", at).Where("id = ?", id).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The listing pages by (updated_at, id): a page before a cursor never repeats
// or skips a row, ties included; pinned sessions ride with the first page;
// a cursor that names no session of the owner is ErrNotFound.
func TestSessionListPage(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	// Nine unpinned sessions on three timestamps (three ties each), two pinned,
	// one hidden, one of another owner.
	var ids []string
	for i := range 9 {
		s := &Session{OwnerID: LocalUserID, ID: NewID(), Name: fmt.Sprintf("s%d", i)}
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		setUpdated(t, db, s.ID, base.Add(time.Duration(i/3)*time.Minute))
		ids = append(ids, s.ID)
	}
	for _, name := range []string{"pin-a", "pin-b"} {
		s := &Session{OwnerID: LocalUserID, ID: NewID(), Name: name, Pinned: true}
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		setUpdated(t, db, s.ID, base.Add(-time.Hour)) // older than every recent
	}
	hidden := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "hidden", Hidden: true}
	if err := sessions.Create(ctx, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewInsert().Model(&User{ID: NewID(), Email: "other@example.com", Role: RoleMember}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	var other User
	if err := db.NewSelect().Model(&other).Where("email = ?", "other@example.com").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	foreign := &Session{OwnerID: other.ID, ID: NewID(), Name: "theirs"}
	if err := sessions.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	all, err := sessions.ListPage(ctx, LocalUserID, SessionPage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 11 || !all[0].Pinned || !all[1].Pinned {
		t.Fatalf("first page = %d rows, pinned first: %v %v", len(all), all[0].Pinned, all[1].Pinned)
	}
	// Walk the unpinned rows four at a time and compare with the whole list.
	var walked []string
	before := ""
	for range 10 {
		page, err := sessions.ListPage(ctx, LocalUserID, SessionPage{Limit: 4, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		if before == "" {
			if len(page) != 6 || !page[0].Pinned || !page[1].Pinned || page[2].Pinned {
				t.Fatalf("first page = %d rows (2 pinned + 4), got pinned flags %v", len(page), []bool{page[0].Pinned, page[1].Pinned, page[2].Pinned})
			}
			page = page[2:]
		}
		for _, s := range page {
			if s.Pinned {
				t.Fatalf("a later page carried pinned %q", s.Name)
			}
			walked = append(walked, s.ID)
		}
		if len(page) < 4 {
			break
		}
		before = page[len(page)-1].ID
	}
	var want []string
	for _, s := range all[2:] {
		want = append(want, s.ID)
	}
	if fmt.Sprint(walked) != fmt.Sprint(want) {
		t.Fatalf("paged walk %v\nwant %v", walked, want)
	}
	seen := map[string]bool{}
	for _, id := range walked {
		if seen[id] {
			t.Fatalf("row %s came back twice", id)
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("row %s was skipped", id)
		}
	}

	if _, err := sessions.ListPage(ctx, LocalUserID, SessionPage{Before: foreign.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's session as a cursor: err = %v, want ErrNotFound", err)
	}
	if _, err := sessions.ListPage(ctx, LocalUserID, SessionPage{Before: NewID()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown cursor: err = %v, want ErrNotFound", err)
	}
}

// A query matches the name or the first user message, case-insensitively,
// with LIKE wildcards taken literally.
func TestSessionListPageQuery(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	mk := func(name, firstUser string) *Session {
		s := &Session{OwnerID: LocalUserID, ID: NewID(), Name: name}
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if firstUser != "" {
			es := NewEntryStoreFor(db, session.Ref{ID: s.ID, Gen: s.Gen})
			seed(t, es, userEntry(t, firstUser), rawEntry(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sure, deploy_now it is"}],"status":"completed"}`), userEntry(t, "later: rollout"))
		}
		return s
	}
	byName := mk("Deploy plan", "")
	byMessage := mk("Untitled", "please write the Rollout checklist")
	mk("Other", "nothing here")
	names := func(page SessionPage) []string {
		rows, err := sessions.ListPage(ctx, LocalUserID, page)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	if got := names(SessionPage{Query: "deploy"}); len(got) != 1 || got[0] != byName.ID {
		t.Fatalf("name match = %v, want %s only (the assistant's deploy_now does not count)", got, byName.ID)
	}
	if got := names(SessionPage{Query: "ROLLOUT CHECK"}); len(got) != 1 || got[0] != byMessage.ID {
		t.Fatalf("first-message match = %v, want %s only (a later user message does not count)", got, byMessage.ID)
	}
	if got := names(SessionPage{Query: "%"}); len(got) != 0 {
		t.Fatalf("a literal %% matched %v", got)
	}
}
