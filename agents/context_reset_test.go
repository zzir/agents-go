package agents_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// resettingStorage is a self-compacting storage whose only pass is a reset:
// everything but the newest user message folds under a checkpoint.
type resettingStorage struct {
	session.Storage
	args []session.CompactionArgs
}

func (s *resettingStorage) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	s.args = append(s.args, args)
	if !args.Reset {
		return nil
	}
	all, err := s.Entries(ctx, session.Cursor{})
	if err != nil {
		return err
	}
	path := session.ActiveBranchOf(all)
	keep := ""
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Kind == session.EntryKindItem && session.ItemRole(path[i].Item) == "user" {
			keep = path[i].ID
			break
		}
	}
	var excluded []string
	for _, e := range path {
		if e.Kind == session.EntryKindItem && e.ID != keep {
			excluded = append(excluded, e.ID)
		}
	}
	cp, err := session.NewCompactionEntry(session.CompactionPayload{
		Summary: session.SummaryMarker + " kept: the parser is Ada's", ExcludedIDs: excluded, Reset: true,
	})
	if err != nil {
		return err
	}
	return s.Append(ctx, cp)
}

func seedParser(t *testing.T, sess *session.Session) {
	t.Helper()
	ctx := context.Background()
	if err := sess.AppendItems(ctx, agents.InputItemsFromText("Who wrote the parser?"), agents.Source{Type: agents.SourceUser}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(ctx, agents.InputItemsFromAssistantText("Ada wrote the parser."), agents.Source{Type: agents.SourceModel}); err != nil {
		t.Fatal(err)
	}
}

func resetScript() *agentstest.FakeModel {
	return agentstest.NewResponseBuilder().
		FunctionCall("new_context", "c1", `{}`).
		NewTurn().
		Text("fresh").
		Build()
}

func inputTexts(req agents.ModelRequest) string {
	var b strings.Builder
	for _, it := range req.Input {
		b.WriteString(session.ItemText(it))
		b.WriteString("|")
	}
	return b.String()
}

// A self-compacting storage gets the reset as a forced pass with Reset set,
// at the save point; the next call reads the fresh context.
func TestNewContextResetsThroughTheStorage(t *testing.T) {
	ctx := context.Background()
	storage := &resettingStorage{Storage: session.NewInMemoryStorage("s")}
	sess := session.NewSession(storage)
	seedParser(t, sess)
	model := resetScript()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{agents.NewContextTool()}}

	res, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatal(err)
	}
	resets := 0
	for _, a := range storage.args {
		if a.Reset {
			resets++
			if !a.Force {
				t.Fatal("a reset is a forced pass")
			}
		}
	}
	if resets != 1 {
		t.Fatalf("resets = %d, want 1 (args: %+v)", resets, storage.args)
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	first, second := inputTexts(reqs[0]), inputTexts(reqs[1])
	if !strings.Contains(first, "Ada wrote the parser.") {
		t.Fatalf("the first call reads the history: %q", first)
	}
	if strings.Contains(second, "Ada wrote the parser.") || !strings.Contains(second, "kept: the parser is Ada's") || !strings.Contains(second, "|go|") {
		t.Fatalf("the second call reads the fresh context: %q", second)
	}
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput && !strings.Contains(strings.ToLower(it.Text()+it.Display().Output), "new context window") {
			t.Fatalf("new_context answered %q", it.Display().Output)
		}
	}
}

// A run-level Compactor resets in memory through ContextResetter and the
// after-run checkpoint records the reset with the carried summary.
func TestNewContextResetsThroughTheCompactor(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	seedParser(t, sess)
	compactor := compaction.New(&compaction.TruncationStrategy{Trigger: compaction.Never()}, nil)
	compactor.ResetSummary = func(context.Context) (string, error) { return "notes: parser by Ada", nil }
	model := resetScript()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{agents.NewContextTool()}}

	if _, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Compaction:   agents.CompactionOptions{Compactor: compactor},
	}); err != nil {
		t.Fatal(err)
	}
	second := inputTexts(model.Requests()[1])
	if strings.Contains(second, "Ada wrote the parser.") || !strings.Contains(second, "notes: parser by Ada") || !strings.Contains(second, "|go|") {
		t.Fatalf("the second call reads the fresh context: %q", second)
	}
	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints int
	for _, e := range entries {
		if e.Kind != session.EntryKindCompaction {
			continue
		}
		checkpoints++
		p, err := e.CompactionPayload()
		if err != nil || !p.Reset || len(p.ExcludedIDs) == 0 {
			t.Fatalf("checkpoint = %+v, %v", p, err)
		}
	}
	if checkpoints != 1 {
		t.Fatalf("checkpoints = %d, want the after-run one", checkpoints)
	}
}

// Without a session that can reset the request is ignored and the run goes on.
func TestNewContextIgnoredWithoutAResettableSession(t *testing.T) {
	ctx := context.Background()
	model := resetScript()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{agents.NewContextTool()}}
	res, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "fresh" || len(model.Requests()) != 2 {
		t.Fatalf("run = %q after %d calls", res.FinalOutputString(), len(model.Requests()))
	}
	if second := inputTexts(model.Requests()[1]); !strings.HasPrefix(second, "go|") {
		t.Fatalf("the context is untouched: %q", second)
	}
	ignored := false
	for _, d := range res.Diagnostics {
		if d.Type == agents.DiagContextResetIgnored {
			ignored = true
		}
	}
	if !ignored {
		t.Fatalf("no context_reset_ignored diagnostic among %+v", res.Diagnostics)
	}
}

// A fresh context refuses another reset until the model does some work:
// the kept user message would otherwise have it ask in every new window.
func TestNewContextNeedsWorkBetweenResets(t *testing.T) {
	ctx := context.Background()
	storage := &resettingStorage{Storage: session.NewInMemoryStorage("s")}
	sess := session.NewSession(storage)
	seedParser(t, sess)
	model := agentstest.NewResponseBuilder().
		FunctionCall("new_context", "c1", `{}`).
		NewTurn().
		FunctionCall("new_context", "c2", `{}`).
		NewTurn().
		FunctionCall("get_time", "c3", `{}`).
		NewTurn().
		FunctionCall("new_context", "c4", `{}`).
		NewTurn().
		Text("done").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{agents.NewContextTool(), timeTool(t)}}
	res, err := agents.RunSync(ctx, agent, "reset now", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatal(err)
	}
	resets := 0
	for _, a := range storage.args {
		if a.Reset {
			resets++
		}
	}
	if resets != 2 {
		t.Fatalf("resets = %d, want 2: the first, then one after the work in between", resets)
	}
	var outputs []string
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput {
			outputs = append(outputs, it.Display().Output)
		}
	}
	if len(outputs) != 4 {
		t.Fatalf("tool outputs = %d: %q", len(outputs), outputs)
	}
	if !strings.Contains(outputs[0], "A new context window starts") || !strings.Contains(outputs[1], "reset just now") || !strings.Contains(outputs[3], "A new context window starts") {
		t.Fatalf("outputs = %q", outputs)
	}
}

// A history limit bounds what the model reads, not what the compactor
// indexes: the reset under a limit is still recorded by the after-run
// checkpoint, so the next run starts from the fresh context too.
func TestNewContextResetsUnderAHistoryLimit(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	for i := range 3 {
		seedParser(t, sess) // six entries, well past the limit below
		_ = i
	}
	compactor := compaction.New(&compaction.TruncationStrategy{Trigger: compaction.Never()}, nil)
	compactor.ResetSummary = func(context.Context) (string, error) { return "notes: parser by Ada", nil }
	model := resetScript()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{agents.NewContextTool()}}
	opts := agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess, Settings: session.Settings{Limit: 4}},
		Compaction:   agents.CompactionOptions{Compactor: compactor},
	}
	if _, err := agents.RunSync(ctx, agent, "go", opts); err != nil {
		t.Fatal(err)
	}
	if second := inputTexts(model.Requests()[1]); strings.Contains(second, "Ada wrote the parser.") || !strings.Contains(second, "notes: parser by Ada") {
		t.Fatalf("the second call reads the fresh context: %q", second)
	}
	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var reset *session.CompactionPayload
	for _, e := range entries {
		if e.Kind == session.EntryKindCompaction {
			p, err := e.CompactionPayload()
			if err != nil {
				t.Fatal(err)
			}
			reset = &p
		}
	}
	if reset == nil || !reset.Reset || len(reset.ExcludedIDs) < 6 {
		t.Fatalf("the reset was not recorded after the run: %+v", reset)
	}
	items, err := sess.ContextItems(ctx, session.Cursor{Limit: -4})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, session.ItemText(it))
	}
	joined := strings.Join(texts, "|")
	if strings.Contains(joined, "Ada wrote the parser.") || !strings.Contains(joined, "notes: parser by Ada") {
		t.Fatalf("the next run would read %q", joined)
	}
}

// A compactor with no strategy folds nothing on its own, but a reset the
// model asked for is still recorded after the run.
func TestNewContextPersistsWithoutAStrategy(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	seedParser(t, sess)
	compactor := compaction.New(nil, nil)
	compactor.ResetSummary = func(context.Context) (string, error) { return "notes: parser by Ada", nil }
	model := resetScript()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{agents.NewContextTool()}}
	if _, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Compaction:   agents.CompactionOptions{Compactor: compactor},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind != session.EntryKindCompaction {
			continue
		}
		p, err := e.CompactionPayload()
		if err != nil {
			t.Fatal(err)
		}
		found = p.Reset && len(p.ExcludedIDs) > 0
	}
	if !found {
		t.Fatal("the reset was not recorded after the run")
	}
	items, err := sess.ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, session.ItemText(it))
	}
	if joined := strings.Join(texts, "|"); strings.Contains(joined, "Ada wrote the parser.") || !strings.Contains(joined, "notes: parser by Ada") {
		t.Fatalf("the next run would read %q", joined)
	}
}

// The reset request and the fresh-context guard ride the paused RunState:
// a new_context that itself waits for approval still resets once the pause
// is over, and a second one after that pause is refused like any other
// second reset without work in between.
func TestNewContextSurvivesAnApprovalPause(t *testing.T) {
	ctx := context.Background()
	storage := &resettingStorage{Storage: session.NewInMemoryStorage("s")}
	sess := session.NewSession(storage)
	seedParser(t, sess)
	model := agentstest.NewResponseBuilder().
		FunctionCall("new_context", "c1", `{}`).
		NewTurn().
		FunctionCall("new_context", "c2", `{}`).
		NewTurn().
		Text("fresh").
		Build()
	gated := agents.NewContextTool()
	gated.NeedsApproval = true
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{gated}}
	opts := agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}}

	res, err := agents.RunSync(ctx, agent, "go", opts)
	if err != nil {
		t.Fatal(err)
	}
	var outputs []string
	for pause := 0; len(res.Interruptions) > 0; pause++ {
		if pause > 2 {
			t.Fatal("more pauses than new_context calls")
		}
		res.State.Approve(res.Interruptions[0], false)
		// Through JSON, as a host that survives a restart would carry it.
		raw, err := res.State.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		state, err := agents.RunStateFromJSON(raw, map[string]*agents.Agent{"a": agent})
		if err != nil {
			t.Fatal(err)
		}
		if res, err = agents.ResumeRunSync(ctx, state, opts); err != nil {
			t.Fatal(err)
		}
		for _, it := range res.NewItems {
			// A resume re-lists the interrupted call's output slot without a
			// value; only what a tool actually answered counts.
			if it.Kind == agents.ItemToolCallOutput && it.Output != nil {
				outputs = append(outputs, fmt.Sprint(it.Output))
			}
		}
	}
	resets := 0
	for _, a := range storage.args {
		if a.Reset {
			resets++
		}
	}
	if resets != 1 {
		t.Fatalf("resets = %d, want exactly one across the two pauses (args %+v)", resets, storage.args)
	}
	if len(outputs) != 2 || !strings.Contains(outputs[0], "new context window starts") || !strings.Contains(outputs[1], "reset just now") {
		t.Fatalf("new_context answered %q", outputs)
	}
	if res.FinalOutputString() != "fresh" {
		t.Fatalf("final = %q", res.FinalOutputString())
	}
}
