package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A scope opens a task's child session only when the task belongs to the
// calling session; anything else is refused before any history is read.
func TestHistorySourceScopesToOwnTasks(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	parent, child, other := store.NewID(), store.NewID(), store.NewID()
	for _, id := range []string{parent, child, other} {
		if err := sessions.Create(ctx, &store.Session{ID: id, OwnerID: store.LocalUserID, Name: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	childRef, err := store.RefFor(ctx, db, child)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.NewEntryStoreFor(db, childRef).Append(ctx, session.Entry{
		Kind: session.EntryKindItem, Source: agents.Source{Type: agents.SourceUser},
		Item: []byte(`{"role":"user","content":"child task input"}`),
	}); err != nil {
		t.Fatal(err)
	}
	lookup := func(_ context.Context, id string) (*store.Task, error) {
		switch id {
		case "mine":
			return &store.Task{ID: id, ParentSessionID: parent, ChildSessionID: child}, nil
		case "theirs":
			return &store.Task{ID: id, ParentSessionID: other, ChildSessionID: child}, nil
		}
		return nil, errors.New("no such task")
	}

	own, err := historySource(ctx, db, lookup, parent, "")
	if err != nil {
		t.Fatal(err)
	}
	if hits, _, err := own.SearchHistory(ctx, session.HistoryQuery{}); err != nil || len(hits) != 0 {
		t.Fatalf("own session is empty: %v %v", hits, err)
	}
	mine, err := historySource(ctx, db, lookup, parent, "mine")
	if err != nil {
		t.Fatal(err)
	}
	hits, _, err := mine.SearchHistory(ctx, session.HistoryQuery{Query: "child"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("a task of this session opens its child: %v %v", hits, err)
	}
	for _, scope := range []string{"theirs", "unknown"} {
		if _, err := historySource(ctx, db, lookup, parent, scope); err == nil || !strings.Contains(err.Error(), "names no task of this conversation") {
			t.Fatalf("scope %q: err = %v", scope, err)
		}
	}
	if _, err := historySource(ctx, db, nil, parent, "mine"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("no task store: err = %v", err)
	}
}
