package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

type lookupArgs struct {
	ID string `json:"id"`
}

// outputTexts lists the run's tool outputs in order, as a renderer sees them.
func outputTexts(items []*agents.RunItem) []string {
	var out []string
	for _, it := range items {
		if it.Kind == agents.ItemToolCallOutput {
			out = append(out, fmt.Sprint(it.Output))
		}
	}
	return out
}

// wireOutputs lists the run's tool outputs in order, as the model read them:
// the output field of each function_call_output item, text parts joined.
func wireOutputs(t *testing.T, items []*agents.RunItem) []string {
	t.Helper()
	var out []string
	for _, it := range items {
		if it.Kind != agents.ItemToolCallOutput {
			continue
		}
		in, err := it.ToInputItem()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := session.MarshalInputItem(in)
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatal(err)
		}
		var text string
		if json.Unmarshal(probe.Output, &text) != nil {
			var parts []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(probe.Output, &parts); err != nil {
				t.Fatalf("output %s is neither text nor parts", probe.Output)
			}
			for _, p := range parts {
				text += p.Text
			}
		}
		out = append(out, text)
	}
	return out
}

// toolLoopStages lists the stages of the run's tool_loop diagnostics.
func toolLoopStages(res *agents.RunResult) []string {
	var out []string
	for _, d := range res.Diagnostics {
		if d.Type == agents.DiagToolLoop {
			out = append(out, fmt.Sprint(d.Details["stage"]))
		}
	}
	return out
}

// A tool that reports a failure — an upstream 400, a record that is not there —
// answered the call. A model working through alternatives is making progress,
// however many of them fail (spec §2.7d).
func TestToolLoop_ReportedErrorsAreNotALoop(t *testing.T) {
	lookup := agents.NewTool("lookup", "", func(_ context.Context, _ *agents.ToolContext, a lookupArgs) (agents.ToolResult, error) {
		res := agents.TextResult("upstream returned 400 for " + a.ID)
		res.IsError = true
		return res, nil
	})
	b := agentstest.NewResponseBuilder()
	for i := range 6 {
		b.FunctionCall("lookup", fmt.Sprintf("c%d", i), fmt.Sprintf(`{"id":"post-%d"}`, i)).NewTurn()
	}
	model := b.Text("the comments endpoint is down").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{lookup}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}})
	if err != nil {
		t.Fatalf("reported errors stopped the run: %v", err)
	}
	agentstest.AssertFinalOutput(t, res, "the comments endpoint is down")
	if stages := toolLoopStages(res); len(stages) != 0 {
		t.Errorf("tool_loop stages = %v, want none", stages)
	}
}

// Polling the same call is progress while its answer changes.
func TestToolLoop_SameCallNewResultIsNotALoop(t *testing.T) {
	n := 0
	status := agents.NewTool("status", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		n++
		return fmt.Sprintf("%d%% done", n*20), nil
	})
	b := agentstest.NewResponseBuilder()
	for i := range 5 {
		b.FunctionCall("status", fmt.Sprintf("c%d", i), `{}`).NewTurn()
	}
	model := b.Text("finished").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{status}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}})
	if err != nil {
		t.Fatal(err)
	}
	if stages := toolLoopStages(res); len(stages) != 0 {
		t.Errorf("tool_loop stages = %v, want none", stages)
	}
}

// The same call returning the same thing is the loop: the third time the model
// is told so, the fourth time its tools are withdrawn and it answers in prose.
// The run ends normally, with the model's own account of where it got to.
func TestToolLoop_RepeatedResultNoticesThenClosesOut(t *testing.T) {
	lookup := agents.NewTool("lookup", "", func(context.Context, *agents.ToolContext, lookupArgs) (string, error) {
		return "no comments", nil
	})
	model := agentstest.NewResponseBuilder().
		FunctionCall("lookup", "c1", `{"id":"p1"}`).NewTurn().
		FunctionCall("lookup", "c2", `{ "id": "p1" }`).NewTurn().
		FunctionCall("lookup", "c3", `{"id":"p1"}`).NewTurn().
		FunctionCall("lookup", "c4", `{"id":"p1"}`).NewTurn().
		Text("p1 has no comments; I stopped asking").
		Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{lookup}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}})
	if err != nil {
		t.Fatalf("err = %v, want the run to close out", err)
	}
	agentstest.AssertFinalOutput(t, res, "p1 has no comments; I stopped asking")
	agentstest.AssertModelCalls(t, model, 5)

	outs := wireOutputs(t, res.NewItems)
	if len(outs) != 4 {
		t.Fatalf("tool outputs = %q, want 4", outs)
	}
	for i, out := range outs[:2] {
		if strings.Contains(out, "Runtime notice") {
			t.Errorf("output %d carries a notice before the limit: %q", i+1, out)
		}
	}
	if !strings.HasPrefix(outs[2], "no comments\n\n[Runtime notice]") || !strings.Contains(outs[2], "3 times") {
		t.Errorf("third output = %q, want the result followed by the notice", outs[2])
	}
	if !strings.Contains(outs[3], "tools are withdrawn") {
		t.Errorf("fourth output = %q, want the closing notice", outs[3])
	}
	// What a renderer shows stays the tool's own output; the diagnostic
	// carries the notice for a person.
	for i, out := range outputTexts(res.NewItems) {
		if out != "no comments" {
			t.Errorf("displayed output %d = %q, want the tool's own", i+1, out)
		}
	}
	if got := strings.Join(toolLoopStages(res), ","); got != "notice,closing_turn" {
		t.Errorf("tool_loop stages = %q, want notice,closing_turn", got)
	}

	// The closing turn keeps the tools defined, and forbids calling them.
	last := model.LastRequest()
	if len(last.Tools) != 1 || last.Settings == nil || last.Settings.ToolChoice != agents.ToolChoiceNone {
		t.Errorf("closing request: %d tools, settings %+v; want the tool with tool_choice none", len(last.Tools), last.Settings)
	}
}

// A tool that cannot run at all — every call errors, whatever the arguments —
// trips the failed-turns valve through the same two stages.
func TestToolLoop_FailedTurnsNoticeThenCloseOut(t *testing.T) {
	down := agents.NewTool("fetch", "", func(context.Context, *agents.ToolContext, lookupArgs) (string, error) {
		return "", errors.New("connection refused")
	})
	b := agentstest.NewResponseBuilder()
	for i := range 4 {
		b.FunctionCall("fetch", fmt.Sprintf("c%d", i), fmt.Sprintf(`{"id":"%d"}`, i)).NewTurn()
	}
	model := b.Text("the fetch service is unreachable").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{down}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}})
	if err != nil {
		t.Fatalf("err = %v, want the run to close out", err)
	}
	agentstest.AssertFinalOutput(t, res, "the fetch service is unreachable")
	outs := wireOutputs(t, res.NewItems)
	if len(outs) != 4 || !strings.Contains(outs[2], "failed to run on 3 consecutive turns") || !strings.Contains(outs[3], "tools are withdrawn") {
		t.Errorf("tool outputs = %q, want the notice on the third and the closing notice on the fourth", outs)
	}
	if got := strings.Join(toolLoopStages(res), ","); got != "notice,closing_turn" {
		t.Errorf("tool_loop stages = %q", got)
	}
}

// A closing turn runs nothing: a model that calls a tool anyway fails the run
// with the trip that withdrew them.
func TestToolLoop_ClosingTurnThatCallsAToolFails(t *testing.T) {
	var calls int
	lookup := agents.NewTool("lookup", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		calls++
		return "same", nil
	})
	b := agentstest.NewResponseBuilder()
	for i := range 5 {
		b.FunctionCall("lookup", fmt.Sprintf("c%d", i), `{}`).NewTurn()
	}
	model := b.Text("unreached").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{lookup}, ModelImpl: model}

	_, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}})
	tle, ok := errors.AsType[*agents.ToolLoopError](err)
	if !ok {
		t.Fatalf("err = %v, want *agents.ToolLoopError", err)
	}
	if tle.Tool != "lookup" || tle.Repeats != 4 {
		t.Errorf("trip = %+v, want lookup repeated 4 times", tle)
	}
	if calls != 4 {
		t.Errorf("tool ran %d times, want 4 — the closing turn's call must not run", calls)
	}
}

// The count rides the pause: a run that tripped once before waiting on a human
// closes out on the next trip after the resume, not on a fresh first notice.
func TestToolLoop_CountSurvivesASerializedPause(t *testing.T) {
	lookup := agents.NewTool("lookup", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "same", nil
	})
	gated := agents.NewTool("gated", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "done", nil
	})
	gated.NeedsApproval = true
	model := agentstest.NewResponseBuilder().
		FunctionCall("lookup", "c1", `{}`).NewTurn().
		FunctionCall("lookup", "c2", `{}`).NewTurn().
		FunctionCall("lookup", "c3", `{}`).NewTurn().
		FunctionCall("gated", "c4", `{}`).NewTurn().
		FunctionCall("lookup", "c5", `{}`).NewTurn().
		Text("closing words").
		Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{lookup, gated}, ModelImpl: model}
	opts := agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}}

	res, err := agents.RunSync(t.Context(), agent, "go", opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.State == nil {
		t.Fatal("want a pause for approval")
	}
	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	state, err := agents.RunStateFromJSON(data, map[string]*agents.Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	state.Approve(state.Interruptions[0], false)

	res, err = agents.ResumeRunSync(t.Context(), state, opts)
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "closing words")
	if got := strings.Join(toolLoopStages(res), ","); got != "closing_turn" {
		t.Errorf("resumed run's tool_loop stages = %q, want closing_turn alone", got)
	}
}

// A repeat counts only while it is recent: the same read three times with
// other work between each is a model re-checking, not a loop.
func TestToolLoop_ARepeatOutsideTheWindowStartsOver(t *testing.T) {
	read := agents.NewTool("read", "", func(context.Context, *agents.ToolContext, lookupArgs) (string, error) {
		return "unchanged", nil
	})
	n := 0
	edit := agents.NewTool("edit", "", func(context.Context, *agents.ToolContext, lookupArgs) (string, error) {
		n++
		return fmt.Sprintf("edited %d", n), nil
	})
	b := agentstest.NewResponseBuilder()
	calls := 0
	for range 3 {
		b.FunctionCall("read", fmt.Sprintf("c%d", calls), `{"id":"main.go"}`).NewTurn()
		calls++
		// Four tool turns of other work: past the window.
		for range 4 {
			b.FunctionCall("edit", fmt.Sprintf("c%d", calls), fmt.Sprintf(`{"id":"f%d"}`, calls)).NewTurn()
			calls++
		}
	}
	model := b.Text("done").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{read, edit}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 40}})
	if err != nil {
		t.Fatal(err)
	}
	if stages := toolLoopStages(res); len(stages) != 0 {
		t.Errorf("tool_loop stages = %v, want none for reads the window apart", stages)
	}
}

// A notice the model heeded expires with the window: a later, unrelated trip
// is a fresh notice, not the closing turn.
func TestToolLoop_AHeededNoticeExpires(t *testing.T) {
	same := agents.NewTool("same", "", func(context.Context, *agents.ToolContext, lookupArgs) (string, error) {
		return "same", nil
	})
	n := 0
	work := agents.NewTool("work", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		n++
		return fmt.Sprintf("step %d", n), nil
	})
	b := agentstest.NewResponseBuilder()
	calls := 0
	turnOf := func(name, args string) {
		b.FunctionCall(name, fmt.Sprintf("c%d", calls), args).NewTurn()
		calls++
	}
	for range 3 {
		turnOf("same", `{"id":"a"}`)
	}
	for range 4 {
		turnOf("work", `{}`)
	}
	for range 3 {
		turnOf("same", `{"id":"b"}`)
	}
	model := b.Text("done").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{same, work}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 40}})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "done")
	if got := strings.Join(toolLoopStages(res), ","); got != "notice,notice" {
		t.Errorf("tool_loop stages = %q, want two notices", got)
	}
}

// A batch that ends the run is not a loop, and its output reaches the caller
// untouched.
func TestToolLoop_ATerminatingBatchIsNotCounted(t *testing.T) {
	stop := agents.NewTool("stop", "", func(context.Context, *agents.ToolContext, struct{}) (agents.ToolResult, error) {
		res := agents.TextResult("final answer")
		res.Terminate = true
		return res, nil
	})
	b := agentstest.NewResponseBuilder()
	model := b.FunctionCall("stop", "c1", `{}`).Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{stop}, ModelImpl: model}
	// Three runs sharing nothing: the valve is per run, so this is about the
	// terminating batch itself under a limit of 1.
	res, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{
		Exec: agents.ExecOptions{ToolLoop: agents.ToolLoopPolicy{MaxRepeatedCalls: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "final answer" {
		t.Errorf("final = %q, want the tool's output without a notice", res.FinalOutputString())
	}
	if stages := toolLoopStages(res); len(stages) != 0 {
		t.Errorf("tool_loop stages = %v, want none", stages)
	}
}

// Queued input continuing a closed-out run gets the tools back: the new input
// is a new attempt.
func TestToolLoop_AFollowUpRestoresTheTools(t *testing.T) {
	lookup := agents.NewTool("lookup", "", func(context.Context, *agents.ToolContext, lookupArgs) (string, error) {
		return "same", nil
	})
	b := agentstest.NewResponseBuilder()
	for i := range 4 {
		b.FunctionCall("lookup", fmt.Sprintf("c%d", i), `{"id":"a"}`).NewTurn()
	}
	model := b.Text("closing words").NewTurn().
		FunctionCall("lookup", "c9", `{"id":"b"}`).NewTurn().
		Text("follow-up answered").
		Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{lookup}, ModelImpl: model}

	stream, ctrl := agents.Run(t.Context(), agent, "go", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}})
	if err := ctrl.FollowUp("now try b"); err != nil {
		t.Fatal(err)
	}
	_, res := agentstest.CollectRun(t, stream)
	agentstest.AssertFinalOutput(t, res, "follow-up answered")
	last := model.LastRequest()
	if last.Settings != nil && last.Settings.ToolChoice == agents.ToolChoiceNone {
		t.Error("the follow-up's turns still forbid tools")
	}
}

// A call a person keeps rejecting answers the model the same way each time,
// and counts like any other repeat.
func TestToolLoop_RejectedRepeatsCount(t *testing.T) {
	gated := agents.NewTool("gated", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "ran", nil
	})
	gated.NeedsApproval = true
	b := agentstest.NewResponseBuilder()
	for i := range 3 {
		b.FunctionCall("gated", fmt.Sprintf("c%d", i), `{}`).NewTurn()
	}
	model := b.Text("gave up").Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{gated}, ModelImpl: model}
	opts := agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 20}}

	res, err := agents.RunSync(t.Context(), agent, "go", opts)
	for range 3 {
		if err != nil {
			t.Fatal(err)
		}
		if res.State == nil {
			break
		}
		res.State.Reject(res.State.Interruptions[0], false, "no")
		res, err = agents.ResumeRunSync(t.Context(), res.State, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "gave up")
	if got := strings.Join(toolLoopStages(res), ","); got != "notice" {
		t.Errorf("tool_loop stages = %q, want one notice on the third rejection", got)
	}
}
