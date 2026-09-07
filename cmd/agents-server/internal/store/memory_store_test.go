package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/uptrace/bun"
)

func mem(kind, id, gen, key, content string) *Memory {
	return &Memory{ScopeKind: kind, ScopeID: id, Gen: gen, Key: key, Content: content, WrittenBy: MemoryWrittenByUser}
}

func keysOf(rows []Memory) string {
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.ScopeKind+":"+r.Key)
	}
	return strings.Join(keys, ",")
}

// What an agent's instructions carry is an allow-list of scopes: global and
// its own agent rows, never a session's.
func TestMemoryInjectableIsAnAllowList(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMemoryStore(db)
	a1, a2 := NewID(), NewID()
	for _, m := range []*Memory{
		mem(MemoryScopeGlobal, "", "", "g", "global"),
		mem(MemoryScopeAgent, a1, "", "mine", "a1"),
		mem(MemoryScopeAgent, a2, "", "other", "a2"),
		mem(MemoryScopeSession, NewID(), "gen", "notes.md", "session"),
	} {
		if err := s.Upsert(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListInjectable(ctx, a1)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(rows); got != "agent:mine,global:g" && got != "global:g,agent:mine" {
		t.Fatalf("injectable = %s", got)
	}
	rows, _ = s.ListInjectable(ctx, "")
	if keysOf(rows) != "global:g" {
		t.Fatalf("no agent: %s", keysOf(rows))
	}
}

// The configuration listing follows agent visibility: a member sees global
// rows and the rows of agents they can see; an admin sees every agent's;
// session rows appear for nobody.
func TestMemoryListConfigFollowsAgentVisibility(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMemoryStore(db)
	agents := NewAgentConfigStore(db)
	alice, bob := NewID(), NewID()
	shared := &AgentConfig{Name: "shared", Model: "m", Scope: ScopeGlobal, OwnerID: alice}
	mineA := &AgentConfig{Name: "alice-private", Model: "m", Scope: ScopePrivate, OwnerID: alice}
	mineB := &AgentConfig{Name: "bob-private", Model: "m", Scope: ScopePrivate, OwnerID: bob}
	for _, ac := range []*AgentConfig{shared, mineA, mineB} {
		if err := agents.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []*Memory{
		mem(MemoryScopeGlobal, "", "", "g", "x"),
		mem(MemoryScopeAgent, shared.ID, "", "s", "x"),
		mem(MemoryScopeAgent, mineA.ID, "", "a", "x"),
		mem(MemoryScopeAgent, mineB.ID, "", "b", "x"),
		mem(MemoryScopeSession, NewID(), "gen", "n", "x"),
	} {
		if err := s.Upsert(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int{"alice": 3, "bob": 3, "admin": 4}
	for who, n := range want {
		caller, admin := alice, false
		switch who {
		case "bob":
			caller = bob
		case "admin":
			caller, admin = NewID(), true
		}
		rows, err := s.ListConfig(ctx, caller, admin, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != n {
			t.Fatalf("%s sees %s, want %d rows", who, keysOf(rows), n)
		}
		for _, r := range rows {
			if r.ScopeKind == MemoryScopeSession {
				t.Fatalf("%s sees a session memory", who)
			}
		}
	}
	rows, _ := s.ListConfig(ctx, bob, false, MemoryScopeAgent, mineA.ID)
	if len(rows) != 0 {
		t.Fatalf("bob narrowed to alice's agent still sees %s", keysOf(rows))
	}
}

// A write is an upsert within the scope's policy; a generation is a scope
// of its own; append creates or extends.
func TestMemoryUpsertAppendAndLimits(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewMemoryStore(db)
	sid := NewID()
	sc := MemoryScope{Kind: MemoryScopeSession, ID: sid, Gen: "g1"}

	first := mem(MemoryScopeSession, sid, "g1", "plan.md", "v1")
	if err := s.Upsert(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	second := mem(MemoryScopeSession, sid, "g1", "plan.md", "v2")
	if err := s.Upsert(ctx, second, nil); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("a replace keeps the row: %s vs %s", second.ID, first.ID)
	}
	got, err := s.GetByKey(ctx, sc, "plan.md")
	if err != nil || got.Content != "v2" {
		t.Fatalf("GetByKey = %+v, %v", got, err)
	}
	if _, err := s.GetByKey(ctx, MemoryScope{Kind: MemoryScopeSession, ID: sid, Gen: "g2"}, "plan.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another generation is another scope: %v", err)
	}
	if err := s.AppendContent(ctx, sc, "log.md", "a", MemoryWrittenByModel, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendContent(ctx, sc, "log.md", "b", MemoryWrittenByModel, "", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetByKey(ctx, sc, "log.md"); got.Content != "ab" || got.WrittenBy != MemoryWrittenByModel {
		t.Fatalf("append = %+v", got)
	}
	rows, _ := s.ListScope(ctx, sc)
	if keysOf(rows) != "session:log.md,session:plan.md" {
		t.Fatalf("ListScope = %s", keysOf(rows))
	}

	big := mem(MemoryScopeAgent, NewID(), "", "big", strings.Repeat("x", MemoryPolicies[MemoryScopeAgent].MaxBytes+1))
	if err := s.Upsert(ctx, big, nil); !errors.Is(err, ErrMemoryTooLarge) {
		t.Fatalf("over the byte limit: %v", err)
	}
	agent := NewID()
	for i := range MemoryPolicies[MemoryScopeAgent].MaxKeys {
		if err := s.Upsert(ctx, mem(MemoryScopeAgent, agent, "", "k"+string(rune('a'+i%26))+strings.Repeat("x", i/26), "v"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Upsert(ctx, mem(MemoryScopeAgent, agent, "", "one-too-many", "v"), nil); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("over the key limit: %v", err)
	}
	if err := s.Upsert(ctx, mem("user", "u", "", "k", "v"), nil); !errors.Is(err, ErrMemoryScope) {
		t.Fatalf("an unknown kind: %v", err)
	}
	guardErr := errors.New("guarded")
	if err := s.Upsert(ctx, mem(MemoryScopeAgent, agent, "", "ka", "v"), func(context.Context, bun.Tx) error { return guardErr }); !errors.Is(err, guardErr) {
		t.Fatalf("the guard runs before the write: %v", err)
	}
}

// Session memory follows the session: a fork gets its own copy, and a
// delete takes the rows with it.
func TestMemoryFollowsTheSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	memories := NewMemoryStore(db)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), OwnerID: LocalUserID, Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatal(err)
	}
	srcRef, err := RefFor(ctx, db, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := memories.Upsert(ctx, mem(MemoryScopeSession, srcRef.ID, srcRef.Gen, "notes.md", "keep"), nil); err != nil {
		t.Fatal(err)
	}
	seed(t, NewEntryStoreFor(db, srcRef), userEntry(t, "hi"))

	dst := &Session{ID: NewID(), OwnerID: LocalUserID, Name: "fork"}
	if _, err := NewEntryStoreFor(db, srcRef).ForkSession(ctx, dst, srcRef, "", false); err != nil {
		t.Fatal(err)
	}
	dstRef, err := RefFor(ctx, db, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := memories.GetByKey(ctx, SessionMemoryScope(dstRef), "notes.md")
	if err != nil || copied.Content != "keep" {
		t.Fatalf("fork copy = %+v, %v", copied, err)
	}
	if err := memories.Upsert(ctx, mem(MemoryScopeSession, dstRef.ID, dstRef.Gen, "notes.md", "changed"), nil); err != nil {
		t.Fatal(err)
	}
	if orig, _ := memories.GetByKey(ctx, SessionMemoryScope(srcRef), "notes.md"); orig.Content != "keep" {
		t.Fatal("writing the fork changed the source")
	}

	if err := sessions.Delete(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := memories.GetByKey(ctx, SessionMemoryScope(srcRef), "notes.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the source's memory survived its delete: %v", err)
	}
	if _, err := memories.GetByKey(ctx, SessionMemoryScope(dstRef), "notes.md"); err != nil {
		t.Fatalf("the fork's memory went with the source: %v", err)
	}
}

func TestAgentDeleteCascadesItsMemory(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	memories := NewMemoryStore(db)
	agents := NewAgentConfigStore(db)
	ac := &AgentConfig{Name: "a", Model: "m", Scope: ScopeGlobal, OwnerID: NewID()}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	if err := memories.Upsert(ctx, mem(MemoryScopeAgent, ac.ID, "", "k", "v"), nil); err != nil {
		t.Fatal(err)
	}
	if err := agents.Delete(ctx, ac.ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ := memories.ListScope(ctx, MemoryScope{Kind: MemoryScopeAgent, ID: ac.ID}); len(rows) != 0 {
		t.Fatalf("agent memory survived the delete: %s", keysOf(rows))
	}
	if err := agents.Delete(ctx, ac.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second delete: %v", err)
	}
}

// The run adapter stamps the model as writer and re-checks the agent's edit
// rule inside the write.
func TestRunMemoryWritesAsTheModel(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	memories := NewMemoryStore(db)
	sessions := NewSessionStore(db)
	alice, bob := NewID(), NewID()
	sess := &Session{ID: NewID(), OwnerID: alice, Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	ac := &AgentConfig{Name: "bobs", Model: "m", Scope: ScopeGlobal, OwnerID: bob}
	if err := NewAgentConfigStore(db).Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	rm := NewRunMemory(db, memories, alice, false)
	sessionScope := memoryScopeOf(MemoryScopeSession, sess.ID)
	if err := rm.Write(ctx, sessionScope, "notes.md", "one"); err != nil {
		t.Fatal(err)
	}
	if err := rm.Append(ctx, sessionScope, "notes.md", "\ntwo"); err != nil {
		t.Fatal(err)
	}
	ref, _ := RefFor(ctx, db, sess.ID)
	row, err := memories.GetByKey(ctx, SessionMemoryScope(ref), "notes.md")
	if err != nil || row.Content != "one\ntwo" || row.WrittenBy != MemoryWrittenByModel || row.OwnerID != alice {
		t.Fatalf("row = %+v, %v", row, err)
	}
	if text, err := rm.Read(ctx, sessionScope, "notes.md"); err != nil || text != "one\ntwo" {
		t.Fatalf("Read = %q, %v", text, err)
	}
	agentScope := memoryScopeOf(MemoryScopeAgent, ac.ID)
	if err := rm.Write(ctx, agentScope, "k", "v"); !errors.Is(err, ErrMemoryForbidden) {
		t.Fatalf("alice writing bob's global agent: %v", err)
	}
	if err := NewRunMemory(db, memories, alice, true).Write(ctx, agentScope, "k", "v"); err != nil {
		t.Fatalf("an admin may: %v", err)
	}
	if err := NewRunMemory(db, memories, bob, false).Write(ctx, agentScope, "k", "v2"); err != nil {
		t.Fatalf("the owner may: %v", err)
	}
}
