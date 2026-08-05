package session_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// touchSession appends one entry, moving the session's UpdatedAt.
func touchSession(t *testing.T, sess *session.Session) {
	t.Helper()
	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(context.Background(), []session.InputItem{item}, session.Source{}); err != nil {
		t.Fatal(err)
	}
}

// A listing comes back newest first and cut to Limit, the same as the file and
// SQL repos. The cut happens after the hidden filter: a task transcript that
// happens to be the newest session must not eat the one slot a caller asked
// for and leave it with nothing.
func TestInMemoryRepoListOrderAndLimit(t *testing.T) {
	ctx := context.Background()
	repo := session.NewInMemoryRepo()

	// Written oldest to newest, hidden one in the middle so the filter and the
	// limit have to compose. The pause keeps the stamps apart — on a coarse
	// clock they would tie, and ties hold creation order, which is the very
	// thing being ruled out.
	for _, s := range []struct {
		id     string
		hidden bool
	}{
		{id: "first"},
		{id: "task", hidden: true},
		{id: "last"},
	} {
		sess, err := repo.Create(ctx, session.CreateOptions{ID: s.id, Hidden: s.hidden})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		touchSession(t, sess)
	}

	visible := []string{"last", "first"}
	for _, tc := range []struct {
		name string
		opts session.ListOptions
		want []string
	}{
		{"newest first", session.ListOptions{}, visible},
		{"hidden included, still newest first", session.ListOptions{IncludeHidden: true}, []string{"last", "task", "first"}},
		{"limit takes the newest", session.ListOptions{Cursor: session.Cursor{Limit: 1}}, []string{"last"}},
		{"limit counts visible sessions", session.ListOptions{Cursor: session.Cursor{Limit: 2}}, visible},
		{"limit past the end", session.ListOptions{Cursor: session.Cursor{Limit: 9}}, visible},
		{"zero is no limit", session.ListOptions{Cursor: session.Cursor{Limit: 0}}, visible},
		{"negative is no limit", session.ListOptions{Cursor: session.Cursor{Limit: -1}}, visible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, err := repo.List(ctx, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(md))
			for _, m := range md {
				got = append(got, m.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("List(%+v) = %v, want %v", tc.opts, got, tc.want)
			}
		})
	}
}
