package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// GET /sessions/:id/runs names every run by the question it started from,
// oldest first — the trace panel's labels for runs whose exchange the paged
// timeline has not loaded — and answers 404 for a session that is not there.
func TestSessionRunsListsQuestions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Under the session's own generation — what the handler resolves the id
	// to — not the direct scope a bare id would write to.
	ref, err := store.RefFor(ctx, db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries := store.NewEntryStoreFor(db, ref)
	user := func(text string) session.Entry {
		q, _ := json.Marshal(text)
		return session.Entry{Kind: session.EntryKindItem, Source: agents.Source{Type: agents.SourceUser},
			Item: json.RawMessage(`{"role":"user","content":` + string(q) + `}`)}
	}
	answer := session.Entry{Kind: session.EntryKindItem,
		Item: json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`)}
	entries.SetRunID("run-a")
	if err := entries.Append(ctx, user("first"), answer); err != nil {
		t.Fatal(err)
	}
	entries.SetRunID("run-b")
	if err := entries.Append(ctx, user("second"), answer); err != nil {
		t.Fatal(err)
	}

	h := NewSessionHandler(testSessionDeps(db))
	engine := newTestEngine()
	engine.GET("/sessions/:id/runs", h.Runs)

	w := doJSON(t, engine, http.MethodGet, "/sessions/"+sess.ID+"/runs", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got []store.RunQuestion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []store.RunQuestion{{RunID: "run-a", Question: "first", OnPath: true}, {RunID: "run-b", Question: "second", OnPath: true}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("runs = %+v, want %+v", got, want)
	}

	if w := doJSON(t, engine, http.MethodGet, "/sessions/nope/runs", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown session: status %d, want 404", w.Code)
	}
}
