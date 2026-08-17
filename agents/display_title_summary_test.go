package agents_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
)

// Title and Summary travel from the tool's return value to the item's display
// projection, and survive the session round-trip — so a reload renders the
// same card the live run showed.
func TestToolResultTitleSummaryReachDisplayAndStore(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	patch := agents.NewTool("apply_patch", "applies a patch",
		func(context.Context, *agents.ToolContext, struct{}) (agents.ToolResult, error) {
			return agents.TextResult("done").
				WithTitle("Apply patch").
				WithSummary("3 files changed"), nil
		})
	model := agentstest.NewResponseBuilder().
		FunctionCall("apply_patch", "call-1", "{}").
		NewTurn().
		Text("applied").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{patch}}

	res, err := agents.RunSync(ctx, agent, "patch it", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatal(err)
	}

	var live *agents.ItemDisplay
	for _, it := range res.NewItems {
		if d := it.Display(); d.Kind == agents.DisplayToolOutput {
			live = &d
		}
	}
	if live == nil {
		t.Fatal("no tool output item in the result")
	}
	if live.Title != "Apply patch" || live.Summary != "3 files changed" {
		t.Errorf("live display = %+v, want the tool's Title/Summary", live)
	}

	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var stored *agents.ItemDisplay
	for _, e := range entries {
		if e.Display != nil && e.Display.Kind == agents.DisplayToolOutput {
			stored = e.Display
		}
	}
	if stored == nil {
		t.Fatal("no tool output display in the stored entries")
	}
	if stored.Title != live.Title || stored.Summary != live.Summary {
		t.Errorf("stored display = %+v, want the same Title/Summary the live run showed", stored)
	}
}

// A tool that sets neither leaves both empty — the consumer's fallback (tool
// name, existing rendering) stays in charge, per the display contract.
func TestToolResultTitleSummaryDefaultEmpty(t *testing.T) {
	plain := agents.NewTool("get_time", "tells the time",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) { return "noon", nil })
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_time", "call-1", "{}").
		NewTurn().
		Text("noon").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{plain}}

	res, err := agents.RunSync(context.Background(), agent, "time?", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.NewItems {
		if d := it.Display(); d.Kind == agents.DisplayToolOutput {
			if d.Title != "" || d.Summary != "" {
				t.Errorf("display = %+v, want empty Title/Summary for a plain tool", d)
			}
		}
	}
}

// A multimodal result displays as the Responses content list — the shape a
// renderer reads by its "type" discriminators — not as the SDK's Go types.
func TestDisplay_MultimodalOutputIsTheWireContentList(t *testing.T) {
	item := &agents.RunItem{Kind: agents.ItemToolCallOutput, Output: []agents.ToolOutputContent{
		agents.ToolOutputText{Text: "shot"},
		agents.ToolOutputImage{ImageURL: "data:image/png;base64,AAAA", Detail: agents.DetailLow},
		agents.ToolOutputFile{FileData: "QUJD", Filename: "a.pdf"},
	}}
	var parts []map[string]any
	if err := json.Unmarshal([]byte(item.Display().Output), &parts); err != nil {
		t.Fatalf("display output is not a JSON list: %v\n%s", err, item.Display().Output)
	}
	want := []map[string]any{
		{"type": "input_text", "text": "shot"},
		{"type": "input_image", "image_url": "data:image/png;base64,AAAA", "detail": "low"},
		{"type": "input_file", "file_data": "QUJD", "filename": "a.pdf"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("display output:\n got %v\nwant %v", parts, want)
	}
	// One part, returned bare, is the same one-element list.
	single := &agents.RunItem{Kind: agents.ItemToolCallOutput, Output: agents.ToolOutputText{Text: "x"}}
	if err := json.Unmarshal([]byte(single.Display().Output), &parts); err != nil || len(parts) != 1 || parts[0]["type"] != "input_text" {
		t.Fatalf("single part display = %s (%v)", single.Display().Output, err)
	}
}
