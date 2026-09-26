package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// busyStopper refuses every fenced write, standing in for a live run.
type busyStopper struct{ noopStopper }

func (busyStopper) WithSessionTreeFenced(context.Context, string, func() error) error {
	return bridge.ErrSessionBusy{RunID: "r1"}
}

// A branch move is refused while a run is live (invariant 19): a mid-run
// switch would graft the run's later turns onto the new branch. When idle,
// the move answers 200 and reports the leaf it left, so the client can roll
// back a regenerate whose run never started.
func TestBranchRefusesLiveRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	ref, err := store.RefFor(ctx, db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries := store.NewEntryStoreFor(db, ref)
	mk := func(role, text string) session.Entry {
		q, _ := json.Marshal(text)
		return session.Entry{Kind: session.EntryKindItem, Source: agents.Source{Type: agents.SourceUser},
			Item: json.RawMessage(`{"role":"` + role + `","content":` + string(q) + `}`)}
	}
	entries.SetRunID(store.NewID())
	if err := entries.Append(ctx, mk("user", "first"), mk("assistant", "a1")); err != nil {
		t.Fatal(err)
	}
	firstLeaf, err := entries.Leaf(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	entries.SetRunID(store.NewID())
	if err := entries.Append(ctx, mk("user", "second"), mk("assistant", "a2")); err != nil {
		t.Fatal(err)
	}
	secondLeaf, err := entries.Leaf(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	// Busy: 409, no move.
	busy := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Stopper = busyStopper{} }))
	engine := newTestEngine()
	engine.POST("/sessions/:id/branch", busy.Branch)
	w := doJSON(t, engine, http.MethodPost, "/sessions/"+sess.ID+"/branch", `{"entry_id":"`+firstLeaf+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("branch while busy: got %d, want 409 (%s)", w.Code, w.Body.String())
	}

	// Idle: 200 with both the new leaf and the leaf it left.
	idle := NewSessionHandler(testSessionDeps(db))
	engine = newTestEngine()
	engine.POST("/sessions/:id/branch", idle.Branch)
	w = doJSON(t, engine, http.MethodPost, "/sessions/"+sess.ID+"/branch", `{"entry_id":"`+firstLeaf+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("branch while idle: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var body struct {
		Leaf         string `json:"leaf"`
		PreviousLeaf string `json:"previous_leaf"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	// The move lands on its target and reports the tip it left, exactly.
	if body.Leaf != firstLeaf || body.PreviousLeaf != secondLeaf {
		t.Errorf("branch = {leaf %q, previous_leaf %q}, want {%q, %q}", body.Leaf, body.PreviousLeaf, firstLeaf, secondLeaf)
	}
}

// fenceProbe is the real runner as the stopper, with one difference: inside
// the fenced write it tries to start a run on the session and records the
// refusal — what a message arriving mid-move would get.
type fenceProbe struct {
	*bridge.Runner
	refused error
}

func (p *fenceProbe) WithSessionTreeFenced(ctx context.Context, id string, fn func() error) error {
	return p.Runner.WithSessionTreeFenced(ctx, id, func() error {
		_, p.refused = p.StartRun(id, "", "", bridge.TextInput("mid-move"), nil, nil)
		return fn()
	})
}

// While a branch move writes, the hub fence — not a snapshot taken earlier —
// keeps a run from starting on the session (invariant 82).
func TestBranchFencesTheSessionAgainstRuns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	ref, err := store.RefFor(ctx, db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries := store.NewEntryStoreFor(db, ref)
	entries.SetRunID(store.NewID())
	q, _ := json.Marshal("first")
	if err := entries.Append(ctx, session.Entry{Kind: session.EntryKindItem, Source: agents.Source{Type: agents.SourceUser},
		Item: json.RawMessage(`{"role":"user","content":` + string(q) + `}`)}); err != nil {
		t.Fatal(err)
	}
	leaf, err := entries.Leaf(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	runner := bridge.NewRunner(t.Context(), db, &bridge.AgentDeps{AgentConfigs: store.NewAgentConfigStore(db), Sessions: sessions, Traces: store.NewTraceStore(db)})
	probe := &fenceProbe{Runner: runner}
	h := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions, d.Stopper = sessions, probe }))
	engine := newTestEngine()
	engine.POST("/sessions/:id/branch", h.Branch)

	if w := doJSON(t, engine, http.MethodPost, "/sessions/"+sess.ID+"/branch", `{"entry_id":"`+leaf+`"}`); w.Code != http.StatusOK {
		t.Fatalf("branch = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !errors.As(probe.refused, new(bridge.ErrSessionBusy)) {
		t.Fatalf("a run starting mid-move got %v, want ErrSessionBusy", probe.refused)
	}
}
