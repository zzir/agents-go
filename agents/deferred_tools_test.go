package agents

import (
	"context"
	"testing"
)

func toolNames(req ModelRequest) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		out = append(out, t.Name)
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// An agent offered forty tools chooses worse than one offered four, and most of
// those forty only matter after something else has happened.
func TestDeferredTools_HiddenUntilDisclosed(t *testing.T) {
	authenticate := NewTool("authenticate", "", func(context.Context, *ToolContext, struct{}) (ToolResult, error) {
		r := TextResult("signed in")
		r.AddedTools = []string{"read_account"}
		return r, nil
	})
	readAccount := NewTool("read_account", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "balance 10", nil
	})
	readAccount.Deferred = true

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "authenticate", "c1", `{}`)),
		modelResp(functionCallOutput(t, "read_account", "c2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{authenticate, readAccount}, ModelImpl: model}

	var offered [][]string
	model.onRequest = func(req ModelRequest) { offered = append(offered, toolNames(req)) }

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "done" {
		t.Fatalf("final = %q", res.FinalOutputString())
	}
	if len(offered) != 3 {
		t.Fatalf("%d model calls, want 3", len(offered))
	}
	if has(offered[0], "read_account") {
		t.Errorf("turn 1 offered the deferred tool: %v", offered[0])
	}
	if !has(offered[1], "read_account") {
		t.Errorf("turn 2 did not offer the disclosed tool: %v", offered[1])
	}
	// Disclosure is cumulative: withdrawing it after one use would surprise a
	// model that had just been told it existed.
	if !has(offered[2], "read_account") {
		t.Errorf("turn 3 withdrew the disclosed tool: %v", offered[2])
	}
}

// Disclosure opens a door, it does not force one: a disclosed tool that is
// disabled stays hidden.
func TestDeferredTools_DisclosureDoesNotOverrideDisabled(t *testing.T) {
	opener := NewTool("open", "", func(context.Context, *ToolContext, struct{}) (ToolResult, error) {
		r := TextResult("ok")
		r.AddedTools = []string{"secret"}
		return r, nil
	})
	secret := NewTool("secret", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "", nil
	})
	secret.Deferred = true
	secret.IsEnabled = func(context.Context, *RunContext, *Agent) (bool, error) { return false, nil }

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "open", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	var offered [][]string
	model.onRequest = func(req ModelRequest) { offered = append(offered, toolNames(req)) }
	agent := &Agent{Name: "a", Tools: []*Tool{opener, secret}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if has(offered[len(offered)-1], "secret") {
		t.Errorf("a disabled tool was offered because it was disclosed: %v", offered)
	}
}

// A tool should not be able to fail a run by mentioning something.
func TestDeferredTools_UnknownNameIsIgnored(t *testing.T) {
	opener := NewTool("open", "", func(context.Context, *ToolContext, struct{}) (ToolResult, error) {
		r := TextResult("ok")
		r.AddedTools = []string{"does_not_exist", ""}
		return r, nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "open", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{opener}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatalf("naming a nonexistent tool failed the run: %v", err)
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// From the model's side, a tool disappearing across an approval pause would
// look like it was taken away mid-conversation.
func TestDeferredTools_DisclosureSurvivesAResume(t *testing.T) {
	opener := NewTool("open", "", func(context.Context, *ToolContext, struct{}) (ToolResult, error) {
		r := TextResult("opened")
		r.AddedTools = []string{"secret"}
		return r, nil
	})
	gated := NewTool("gated", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "done", nil
	})
	gated.NeedsApproval = true
	secret := NewTool("secret", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "", nil
	})
	secret.Deferred = true

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "open", "c1", `{}`)),
		modelResp(functionCallOutput(t, "gated", "c2", `{}`)),
		modelResp(messageOutput(t, "finished")),
	}}
	var offered [][]string
	model.onRequest = func(req ModelRequest) { offered = append(offered, toolNames(req)) }
	agent := &Agent{Name: "a", Tools: []*Tool{opener, gated, secret}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected an approval pause, got %+v", res.Interruptions)
	}
	if len(res.State.DisclosedTools) != 1 || res.State.DisclosedTools[0] != "secret" {
		t.Fatalf("state carries %v, want [secret]", res.State.DisclosedTools)
	}

	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRunSync(context.Background(), res.State, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !has(offered[len(offered)-1], "secret") {
		t.Errorf("the resumed run re-hid a disclosed tool: %v", offered[len(offered)-1])
	}
}
