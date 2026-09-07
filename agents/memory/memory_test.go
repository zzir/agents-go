package memory_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/memory"
	"github.com/zzir/agents-go/internal/agentstest"
)

var (
	sessionScope = memory.Scope{Kind: "session", ID: "s1"}
	agentScope   = memory.Scope{Kind: "agent", ID: "a1"}
)

func specs() []memory.ScopeSpec {
	return []memory.ScopeSpec{
		{Scope: sessionScope, Name: "session", Writable: true, Describe: "this conversation's working notes.", MaxBytes: 64, MaxKeys: 2},
		{Scope: agentScope, Name: "agent", Writable: true, Approve: true, Describe: "what future conversations should know."},
		{Scope: memory.Scope{Kind: "global"}, Name: "global", Describe: "shared by every agent."},
	}
}

func toolOutputs(res *agents.RunResult) []string {
	var out []string
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput {
			out = append(out, fmt.Sprint(it.Output))
		}
	}
	return out
}

func call(name, id, args string) agentstest.Turn {
	return agentstest.Turn{Items: []agents.OutputItem{agentstest.FunctionCallItem("fc_"+id, name, id, args)}}
}

// The model writes, appends, lists, reads a line range and searches its
// session memory; the store holds what it wrote.
func TestMemoryToolsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := memory.NewInMemoryStore()
	model := agentstest.NewFakeModel(
		call("memory_write", "c1", `{"scope":"","key":"plan.md","text":"one\ntwo"}`),
		call("memory_append", "c2", `{"scope":"session","key":"plan.md","text":"\nthree"}`),
		call("memory_list", "c3", `{"scope":""}`),
		call("memory_read", "c4", `{"scope":"","key":"plan.md","start_line":2,"end_line":-1}`),
		call("memory_search", "c5", `{"scope":"","query":"TWO","key_prefix":"","max_files":0,"max_matches_per_file":0}`),
		call("memory_read", "c6", `{"scope":"","key":"nope","start_line":0,"end_line":0}`),
		agentstest.Turn{Items: []agents.OutputItem{agentstest.MessageItem("m", "done")}},
	)
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: memory.Tools(specs(), memory.Static(store))}
	res, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 10}})
	if err != nil {
		t.Fatal(err)
	}
	outs := toolOutputs(res)
	if len(outs) != 6 {
		t.Fatalf("outputs = %d: %q", len(outs), outs)
	}
	if !strings.HasPrefix(outs[0], "Wrote plan.md in session (7 bytes now)") {
		t.Fatalf("write: %q", outs[0])
	}
	if !strings.HasPrefix(outs[1], "Appended to plan.md in session (13 bytes now)") {
		t.Fatalf("append: %q", outs[1])
	}
	if !strings.Contains(outs[2], "1 memories in session") || !strings.Contains(outs[2], "plan.md (13 bytes") {
		t.Fatalf("list: %q", outs[2])
	}
	if outs[3] != "Lines 2-3 of 3:\ntwo\nthree" {
		t.Fatalf("read range: %q", outs[3])
	}
	if !strings.Contains(outs[4], "plan.md:\n  2: two") {
		t.Fatalf("search: %q", outs[4])
	}
	if outs[5] != `No memory "nope" in session.` {
		t.Fatalf("missing: %q", outs[5])
	}
	if text, _ := store.Read(ctx, sessionScope, "plan.md"); text != "one\ntwo\nthree" {
		t.Fatalf("stored = %q", text)
	}
}

// Writes obey the scope: read-only scopes refuse, limits refuse, and only
// an Approve scope asks for approval.
func TestMemoryToolsScopeRules(t *testing.T) {
	ctx := context.Background()
	store := memory.NewInMemoryStore()
	model := agentstest.NewFakeModel(
		call("memory_write", "c1", `{"scope":"global","key":"k","text":"x"}`),
		call("memory_write", "c2", `{"scope":"nowhere","key":"k","text":"x"}`),
		call("memory_write", "c3", `{"scope":"","key":"../k","text":"x"}`),
		call("memory_write", "c4", `{"scope":"","key":"big","text":"`+strings.Repeat("x", 65)+`"}`),
		call("memory_write", "c5", `{"scope":"","key":"a","text":"x"}`),
		call("memory_write", "c6", `{"scope":"","key":"b","text":"x"}`),
		call("memory_write", "c7", `{"scope":"","key":"c","text":"x"}`),
		agentstest.Turn{Items: []agents.OutputItem{agentstest.MessageItem("m", "done")}},
	)
	tools := memory.Tools(specs(), memory.Static(store))
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: tools}
	res, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 10}})
	if err != nil {
		t.Fatal(err)
	}
	outs := toolOutputs(res)
	want := []string{"global is read-only for you", `No memory scope "nowhere"`, "empty, . or .. segment", "allows 64 per memory", "Wrote a", "Wrote b", "already holds 2 memories"}
	for i, w := range want {
		if !strings.Contains(outs[i], w) {
			t.Fatalf("output %d = %q, want it to contain %q", i, outs[i], w)
		}
	}

	var write *agents.Tool
	for _, tool := range tools {
		if tool.Name == "memory_write" {
			write = tool
		}
		if tool.Name == "memory_list" || tool.Name == "memory_read" || tool.Name == "memory_search" {
			if !tool.ReadOnly {
				t.Fatalf("%s must be read-only", tool.Name)
			}
		}
	}
	for args, want := range map[string]bool{
		`{"scope":"agent","key":"k","text":"x"}`:   true,
		`{"scope":"session","key":"k","text":"x"}`: false,
		`{"scope":"","key":"k","text":"x"}`:        false,
		`{"scope":"global","key":"k","text":"x"}`:  false,
		`not json`: false,
	} {
		got, err := write.NeedsApprovalFunc(ctx, nil, args, "c")
		if err != nil || got != want {
			t.Fatalf("NeedsApproval(%s) = %v, %v; want %v", args, got, err, want)
		}
	}
}

func TestValidKey(t *testing.T) {
	for _, ok := range []string{"plan.md", "findings/auth.md", "a b", "日本語"} {
		if err := memory.ValidKey(ok); err != nil {
			t.Errorf("ValidKey(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "a//b", "./x", "a/../b", "x\ny", strings.Repeat("k", 201)} {
		if err := memory.ValidKey(bad); err == nil {
			t.Errorf("ValidKey(%q) accepted", bad)
		}
	}
}

func TestSnapshotFitsThenLists(t *testing.T) {
	ctx := context.Background()
	store := memory.NewInMemoryStore()
	_ = store.Write(ctx, sessionScope, "b.md", "bee")
	_ = store.Write(ctx, sessionScope, "a.md", "ay")
	_ = store.Write(ctx, sessionScope, "c.md", strings.Repeat("c", 100))

	whole, err := memory.Snapshot(ctx, store, sessionScope, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(whole, "## a.md\nay\n\n## b.md\nbee\n\n## c.md\n") || strings.Contains(whole, "Not shown") {
		t.Fatalf("whole = %q", whole)
	}
	cut, err := memory.Snapshot(ctx, store, sessionScope, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cut, "## a.md\nay") || !strings.Contains(cut, "Not shown (read with memory_read)\n- c.md (100 bytes)") || len(cut) > 100 {
		t.Fatalf("cut = %q (%d bytes)", cut, len(cut))
	}
	empty, err := memory.Snapshot(ctx, store, agentScope, 0)
	if err != nil || empty != "" {
		t.Fatalf("empty = %q, %v", empty, err)
	}
}

// The list of what was not shown counts against the bound too: a hundred
// long keys never push the snapshot past maxChars.
func TestSnapshotNeverExceedsTheBound(t *testing.T) {
	ctx := context.Background()
	store := memory.NewInMemoryStore()
	for i := range 100 {
		key := fmt.Sprintf("%03d-%s.md", i, strings.Repeat("k", 190))
		if err := store.Write(ctx, sessionScope, key, strings.Repeat("x", 500)); err != nil {
			t.Fatal(err)
		}
	}
	for _, bound := range []int{20_000, 2_000, 200, 30} {
		snap, err := memory.Snapshot(ctx, store, sessionScope, bound)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap) > bound {
			t.Fatalf("bound %d: snapshot is %d bytes", bound, len(snap))
		}
		if bound >= 200 && !strings.Contains(snap, " more") {
			t.Fatalf("bound %d: the cut list says how many were left out: %q", bound, snap[len(snap)-60:])
		}
	}
}
