package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// An output item type the SDK does not model must survive a turn byte for byte.
// It used to be dropped by processModelResponse's default branch — which does
// not merely lose a feature, it corrupts the conversation, because the next
// turn resends a history the model does not recognize as its own.
func TestUnknownOutputItem_RoundTripsByteForByte(t *testing.T) {
	const raw = `{"type":"some_future_call","id":"fx_1","status":"completed","weird":{"a":[1,2],"b":null}}`
	var unknown OutputItem
	if err := json.Unmarshal([]byte(raw), &unknown); err != nil {
		t.Fatal(err)
	}

	// Alongside a tool call, so the run takes a second turn and resends the
	// history — which is where a dropped item does its damage.
	tool := NewTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "ok", nil })
	model := &fakeModel{responses: []*ModelResponse{
		{
			Output: []OutputItem{unknown, functionCallOutput(t, "t", "c1", `{}`)},
			Usage:  NewUsage(),
		},
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// It is kept as an item, not swallowed.
	var found *RunItem
	for _, it := range res.NewItems {
		if it.Kind == ItemUnknown {
			found = it
		}
	}
	if found == nil {
		t.Fatalf("the unknown item was dropped; got %v", itemTypesOf(res.NewItems))
	}
	if string(found.Kind) != "unknown" || found.Display().Kind != DisplayUnknown {
		t.Errorf("ItemType/Display = %q/%q", string(found.Kind), found.Display().Kind)
	}
	if found.Display().Text != "some_future_call" {
		t.Errorf("Display().Text = %q, want the wire type name", found.Display().Text)
	}

	// And the second turn resent it verbatim.
	sent, err := json.Marshal(model.lastReq.Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"some_future_call"`, `"fx_1"`, `"weird"`, `[1,2]`} {
		if !strings.Contains(string(sent), fragment) {
			t.Errorf("turn 2 input lost %s\ngot: %s", fragment, sent)
		}
	}
}

// Direct conversion, independent of the run loop: a known type keeps its typed
// form (downstream code inspects OfReasoning and friends), an unknown one goes
// through the raw override.
func TestOutputItemToInput_TypedFirstOverrideFallback(t *testing.T) {
	var reasoning OutputItem
	if err := json.Unmarshal([]byte(`{"type":"reasoning","id":"r1","summary":[]}`), &reasoning); err != nil {
		t.Fatal(err)
	}
	in, err := outputItemToInput(reasoning)
	if err != nil {
		t.Fatal(err)
	}
	if in.OfReasoning == nil {
		t.Error("a reasoning item lost its typed form; applyReasoningItemIDPolicy needs it")
	}

	var unknown OutputItem
	if err := json.Unmarshal([]byte(`{"type":"brand_new","id":"x1","payload":{"n":7}}`), &unknown); err != nil {
		t.Fatal(err)
	}
	in2, err := outputItemToInput(unknown)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(in2)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"brand_new"`, `"x1"`, `"n":7`} {
		if !strings.Contains(string(b), fragment) {
			t.Errorf("override lost %s: %s", fragment, b)
		}
	}
}

// Every item reports where it came from. This is what replaced the sentinel id
// the SDK used to stamp on synthesized items.
func TestSource_PropagatesThroughARun(t *testing.T) {
	tool := NewTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "out", nil })
	billing := &Agent{Name: "billing", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "handled")),
	}}}
	triage := &Agent{
		Name:     "triage",
		Tools:    []*Tool{tool},
		Handoffs: []Handoff{HandoffTo(billing)},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "t", "c1", `{}`)),
			modelResp(functionCallOutput(t, "transfer_to_billing", "h1", `{}`)),
		}},
	}

	res, err := RunSync(context.Background(), triage, "hi", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]SourceType{
		"tool_call":        SourceModel,
		"tool_call_output": SourceTool,
		"handoff_call":     SourceModel,
		"handoff_output":   SourceHandoff,
		"message_output":   SourceModel,
	}
	seen := map[string]bool{}
	for _, it := range res.NewItems {
		kind := string(it.Kind)
		if w, ok := want[kind]; ok {
			seen[kind] = true
			if got := it.Source.Type; got != w {
				t.Errorf("%s source = %q, want %q", kind, got, w)
			}
		}
	}
	for kind := range want {
		if !seen[kind] {
			t.Errorf("run produced no %s item; got %v", kind, itemTypesOf(res.NewItems))
		}
	}
}

// An error handler's fallback message is the SDK's, not the model's — which is
// what hasOffChainItems reads to find the last item a response chain can hold,
// and what the sentinel id used to encode.
func TestSource_ErrorHandlerFallback(t *testing.T) {
	loop := NewTool("loop", "loops",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "again", nil })
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{loop}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{
			MaxTurns: 1,
			ErrorHandlers: RunErrorHandlers{
				MaxTurns: func(context.Context, RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
					return &RunErrorHandlerResult{FinalOutput: "gave up"}, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var fallback *RunItem
	for _, it := range res.NewItems {
		if it.Kind == ItemMessage && it.Text() == "gave up" {
			fallback = it
		}
	}
	if fallback == nil {
		t.Fatalf("no fallback message; got %v", itemTypesOf(res.NewItems))
	}
	src := fallback.Source
	if src.Type != SourceErrorHandler {
		t.Errorf("fallback source = %q, want error_handler", src.Type)
	}
	if src.ID != "max_turns" {
		t.Errorf("fallback source id = %q, want the handler kind", src.ID)
	}
	// It carries no id at all now — the sentinel is gone, not renamed.
	if fallback.Raw.ID != "" {
		t.Errorf("synthesized message has id %q; it should have none", fallback.Raw.ID)
	}
}

// Display is what a consumer renders from, so it must be populated without any
// wire-format parsing on their side.
func TestDisplay_ProjectsEveryItemKind(t *testing.T) {
	tool := NewTool("get_weather", "w",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "sunny", nil })
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "c1", `{"city":"Paris"}`)),
		modelResp(messageOutput(t, "it is sunny")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	byKind := map[string]ItemDisplay{}
	for _, it := range res.NewItems {
		byKind[it.Display().Kind] = it.Display()
	}

	call := byKind[DisplayToolCall]
	if call.ToolName != "get_weather" || call.CallID != "c1" || call.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool_call display = %+v", call)
	}
	out := byKind[DisplayToolOutput]
	if out.Output != "sunny" || out.CallID != "c1" {
		t.Errorf("tool_output display = %+v", out)
	}
	if msg := byKind[DisplayMessage]; msg.Text != "it is sunny" {
		t.Errorf("message display = %+v", msg)
	}
}

func itemTypesOf(items []*RunItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = string(it.Kind)
	}
	return out
}

// An unknown item that reaches a session must be readable back. Before this
// step unknown items were dropped, so they never got stored; now they do, and a
// session that could not be re-read would make the whole conversation
// unloadable over one item.
func TestUnknownItem_SurvivesSessionRoundTrip(t *testing.T) {
	const raw = `{"type":"some_future_call","id":"fx_1","weird":{"a":[1,2]}}`
	var unknown OutputItem
	if err := json.Unmarshal([]byte(raw), &unknown); err != nil {
		t.Fatal(err)
	}
	in, err := outputItemToInput(unknown)
	if err != nil {
		t.Fatal(err)
	}

	stored, err := session.MarshalInputItem(in)
	if err != nil {
		t.Fatal(err)
	}
	back, err := session.UnmarshalInputItem(stored)
	if err != nil {
		t.Fatalf("a stored unknown item could not be read back: %v", err)
	}
	again, err := session.MarshalInputItem(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != raw {
		t.Errorf("session round-trip changed the bytes:\n got %s\nwant %s", again, raw)
	}

	// Malformed JSON with no type is still an error, not an opaque blob.
	if _, err := session.UnmarshalInputItem([]byte(`{"nonsense":true}`)); err == nil {
		t.Error("an item with no type should be rejected, not stored as an override")
	}
}

// End to end: a run whose model emits an unknown item persists it, and the next
// run reads the session back and resends it.
func TestUnknownItem_ReplaysFromSession(t *testing.T) {
	const raw = `{"type":"some_future_call","id":"fx_1","weird":{"a":[1,2]}}`
	var unknown OutputItem
	if err := json.Unmarshal([]byte(raw), &unknown); err != nil {
		t.Fatal(err)
	}
	sess := session.NewInMemorySession()
	tool := NewTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "ok", nil })

	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		{Output: []OutputItem{unknown, functionCallOutput(t, "t", "c1", `{}`)}, Usage: NewUsage()},
		modelResp(messageOutput(t, "done")),
	}}}
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(sess)},
	}); err != nil {
		t.Fatal(err)
	}

	// A second run loads the history — which is where an unreadable item would
	// fail the whole conversation.
	next := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "second"))}}
	if _, err := RunSync(context.Background(), &Agent{Name: "a", ModelImpl: next}, "again", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(sess)},
	}); err != nil {
		t.Fatalf("reloading a session holding an unknown item failed: %v", err)
	}
	sent, err := json.Marshal(next.lastReq.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), "some_future_call") {
		t.Errorf("the reloaded history lost the unknown item: %s", sent)
	}
}
