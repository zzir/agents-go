package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// refusalOutput builds an assistant message whose content is a refusal part.
func refusalOutput(t *testing.T, refusal string) TResponseOutputItem {
	t.Helper()
	raw := `{"type":"message","id":"msg_r","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":` +
		quote(refusal) + `}]}`
	return mustOutputItem(t, raw)
}

// textAndRefusalOutput builds an assistant message carrying both an
// output_text part and a refusal part.
func textAndRefusalOutput(t *testing.T, text, refusal string) TResponseOutputItem {
	t.Helper()
	raw := `{"type":"message","id":"msg_tr","status":"completed","role":"assistant","content":[` +
		`{"type":"output_text","text":` + quote(text) + `,"annotations":[]},` +
		`{"type":"refusal","refusal":` + quote(refusal) + `}]}`
	return mustOutputItem(t, raw)
}

// messageTexts collects the text of every MessageOutputItem in items.
func messageTexts(items []RunItem) []string {
	var out []string
	for _, it := range items {
		if m, ok := it.(*MessageOutputItem); ok {
			out = append(out, m.Text())
		}
	}
	return out
}

func TestErrorHandlers_InvalidFinalOutput_FailsWithoutHandler(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("error = %v, want *ModelBehaviorError", err)
	}
}

func TestErrorHandlers_InvalidFinalOutput_RecoversValidatedFallback(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	var seen RunErrorHandlerInput
	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			seen = in
			return &RunErrorHandlerResult{FinalOutput: sentiment{Label: "fallback", Score: 1}}, nil
		},
	}}}
	res, err := RunSync(context.Background(), agent, "hi", opts)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := FinalOutputAs[sentiment](res)
	if !ok || got.Label != "fallback" || got.Score != 1 {
		t.Errorf("final output = %#v", res.FinalOutput)
	}
	// The handler saw the validation failure and the run snapshot.
	var mbe *ModelBehaviorError
	if !errors.As(seen.Error, &mbe) {
		t.Errorf("handler error = %v, want *ModelBehaviorError", seen.Error)
	}
	if len(seen.RunData.RawResponses) != 1 {
		t.Errorf("handler raw responses = %d, want 1", len(seen.RunData.RawResponses))
	}
	if texts := messageTexts(seen.RunData.NewItems); len(texts) != 1 || texts[0] != "not valid json" {
		t.Errorf("handler new item texts = %q", texts)
	}
	if seen.RunData.LastAgent != agent {
		t.Errorf("handler last agent mismatch")
	}
	if len(seen.RunData.History) != len(seen.RunData.Input)+len(seen.RunData.Output) {
		t.Errorf("history = %d items, want input(%d) + output(%d)",
			len(seen.RunData.History), len(seen.RunData.Input), len(seen.RunData.Output))
	}
	// The invalid message stays, followed by the synthesized fallback message.
	texts := messageTexts(res.NewItems)
	if len(texts) != 2 || texts[0] != "not valid json" || texts[1] != `{"label":"fallback","score":1}` {
		t.Errorf("result message texts = %q", texts)
	}
}

func TestErrorHandlers_InvalidFinalOutput_ExcludeFromHistory(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{
				FinalOutput:        sentiment{Label: "fallback", Score: 1},
				ExcludeFromHistory: true,
			}, nil
		},
	}}}
	res, err := RunSync(context.Background(), agent, "hi", opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := FinalOutputAs[sentiment](res); got.Label != "fallback" {
		t.Errorf("final output = %#v", res.FinalOutput)
	}
	if texts := messageTexts(res.NewItems); len(texts) != 1 || texts[0] != "not valid json" {
		t.Errorf("message texts = %q, want only the model message", texts)
	}
}

func TestErrorHandlers_InvalidFinalOutput_RejectsInvalidFallback(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{FinalOutput: map[string]any{"unexpected": "value"}}, nil
		},
	}}}
	_, err := RunSync(context.Background(), agent, "hi", opts)
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v, want *UserError", err)
	}
	if !strings.Contains(err.Error(), "run error handler") {
		t.Errorf("error message = %q", err.Error())
	}
}

func TestErrorHandlers_InvalidFinalOutput_CanDecline(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return nil, nil
		},
	}}}
	_, err := RunSync(context.Background(), agent, "hi", opts)
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("error = %v, want the original *ModelBehaviorError", err)
	}
}

func TestErrorHandlers_InvalidFinalOutput_IgnoresOtherModelBehaviorErrors(t *testing.T) {
	// A call to a missing tool is a ModelBehaviorError too, but must not be
	// routed to the invalid_final_output handler.
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "missing_tool", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	handlerCalled := false
	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			handlerCalled = true
			return &RunErrorHandlerResult{FinalOutput: sentiment{Label: "x", Score: 0}}, nil
		},
	}}}
	_, err := RunSync(context.Background(), agent, "hi", opts)
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("error = %v, want *ModelBehaviorError", err)
	}
	if handlerCalled {
		t.Error("invalid_final_output handler must not fire for unknown-tool errors")
	}
}

func TestErrorHandlers_EmptyStructuredOutput_RunsAgainWithoutHandler(t *testing.T) {
	for name, first := range map[string]*ModelResponse{
		"no message":    modelResp(),
		"empty message": modelResp(messageOutput(t, "")),
	} {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []*ModelResponse{
				first,
				modelResp(messageOutput(t, `{"label":"ok","score":5}`)),
			}}
			agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

			res, err := RunSync(context.Background(), agent, "hi", RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := FinalOutputAs[sentiment](res); got.Label != "ok" {
				t.Errorf("final output = %#v", res.FinalOutput)
			}
			if model.calls != 2 {
				t.Errorf("model calls = %d, want 2 (empty output runs the model again)", model.calls)
			}
		})
	}
}

func TestErrorHandlers_EmptyStructuredOutput_RecoversWithoutAnotherTurn(t *testing.T) {
	for name, first := range map[string]*ModelResponse{
		"no message":    modelResp(),
		"empty message": modelResp(messageOutput(t, "")),
	} {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []*ModelResponse{
				first,
				modelResp(messageOutput(t, `{"label":"unused","score":0}`)),
			}}
			agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

			opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
				InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
					return &RunErrorHandlerResult{FinalOutput: sentiment{Label: "fallback", Score: 1}}, nil
				},
			}}}
			res, err := RunSync(context.Background(), agent, "hi", opts)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := FinalOutputAs[sentiment](res); got.Label != "fallback" {
				t.Errorf("final output = %#v", res.FinalOutput)
			}
			if model.calls != 1 {
				t.Errorf("model calls = %d, want 1 (handler recovery skips the extra turn)", model.calls)
			}
		})
	}
}

func TestErrorHandlers_ModelRefusal_Recovers(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(refusalOutput(t, "cannot help"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	var seenErr error
	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		ModelRefusal: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			seenErr = in.Error
			return &RunErrorHandlerResult{FinalOutput: "safe fallback"}, nil
		},
	}}}
	res, err := RunSync(context.Background(), agent, "hi", opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "safe fallback" {
		t.Errorf("final output = %q", res.FinalOutputString())
	}
	var re *ModelRefusalError
	if !errors.As(seenErr, &re) || re.Refusal != "cannot help" {
		t.Errorf("handler error = %v, want *ModelRefusalError with refusal text", seenErr)
	}
	// The synthesized plain-text fallback message joins the items.
	if texts := messageTexts(res.NewItems); len(texts) != 2 || texts[1] != "safe fallback" {
		t.Errorf("message texts = %q", texts)
	}
}

func TestErrorHandlers_ModelRefusal_FailsWithoutHandler(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(refusalOutput(t, "cannot help"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	var re *ModelRefusalError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want *ModelRefusalError", err)
	}
	if re.Refusal != "cannot help" {
		t.Errorf("refusal = %q", re.Refusal)
	}
}

func TestErrorHandlers_RefusalTakesPrecedenceOverText(t *testing.T) {
	// A message carrying both text and a refusal part is a refusal (Python
	// parity: extract_refusal wins over extract_text).
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(textAndRefusalOutput(t, "partial text", "cannot help")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	var re *ModelRefusalError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want *ModelRefusalError", err)
	}
}

func TestErrorHandlers_MaxTurns_Recovers(t *testing.T) {
	tool := NewFunctionTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c3", `{}`)),
	}}
	var endOutput any
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model,
		OnEnd: func(_ context.Context, _ *RunContext, output any) error {
			endOutput = output
			return nil
		}}

	var seen RunErrorHandlerInput
	opts := RunOptions{Exec: ExecOptions{MaxTurns: 2, ErrorHandlers: RunErrorHandlers{
		MaxTurns: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			seen = in
			return &RunErrorHandlerResult{FinalOutput: "ran out of turns"}, nil
		},
	}}}
	res, err := RunSync(context.Background(), agent, "go", opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "ran out of turns" {
		t.Errorf("final output = %q", res.FinalOutputString())
	}
	var mte *MaxTurnsError
	if !errors.As(seen.Error, &mte) || mte.MaxTurns != 2 {
		t.Errorf("handler error = %v, want *MaxTurnsError{MaxTurns: 2}", seen.Error)
	}
	if len(seen.RunData.RawResponses) != 2 {
		t.Errorf("handler raw responses = %d, want 2", len(seen.RunData.RawResponses))
	}
	// The synthesized message is the last item, and the end hook saw the
	// fallback output.
	if texts := messageTexts(res.NewItems); len(texts) != 1 || texts[0] != "ran out of turns" {
		t.Errorf("message texts = %q", texts)
	}
	if endOutput != "ran out of turns" {
		t.Errorf("OnAgentEnd output = %v", endOutput)
	}
}

func TestErrorHandlers_MaxTurns_DeclineKeepsError(t *testing.T) {
	tool := NewFunctionTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{MaxTurns: 1, ErrorHandlers: RunErrorHandlers{
		MaxTurns: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return nil, nil
		},
	}}}
	_, err := RunSync(context.Background(), agent, "go", opts)
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("error = %v, want ErrMaxTurns", err)
	}
}

func TestErrorHandlers_HandlerErrorAbortsRun(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(refusalOutput(t, "nope"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	boom := errors.New("handler exploded")
	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		ModelRefusal: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return nil, boom
		},
	}}}
	_, err := RunSync(context.Background(), agent, "hi", opts)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the handler's own error", err)
	}
}

func TestErrorHandlers_WrappedOutputType_WrapsFallback(t *testing.T) {
	// OutputType[[]string] uses the {"response": ...} envelope; the handler
	// returns the bare value and the runner wraps it for validation and the
	// synthesized message.
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[[]string](), ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{FinalOutput: []string{"a", "b"}}, nil
		},
	}}}
	res, err := RunSync(context.Background(), agent, "hi", opts)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FinalOutputAs[[]string](res)
	if !ok || len(got) != 2 || got[0] != "a" {
		t.Errorf("final output = %#v", res.FinalOutput)
	}
	texts := messageTexts(res.NewItems)
	if len(texts) != 2 || texts[1] != `{"response":["a","b"]}` {
		t.Errorf("message texts = %q", texts)
	}
}

func TestErrorHandlers_RecoveredMessagePersistsToSession(t *testing.T) {
	session := NewInMemorySession()
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	opts := RunOptions{Conversation: ConversationOptions{Session: NewSession(session)}, Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{FinalOutput: sentiment{Label: "fallback", Score: 1}}, nil
		},
	}}}
	if _, err := RunSync(context.Background(), agent, "hi", opts); err != nil {
		t.Fatal(err)
	}
	items, err := session.ContextItems(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	// user input + model message + synthesized fallback message
	if len(items) != 3 {
		t.Fatalf("session items = %d, want 3", len(items))
	}
	last := items[len(items)-1]
	if last.OfOutputMessage == nil {
		t.Fatalf("last session item is not an output message: %+v", last)
	}
}

func TestErrorHandlers_MaxTurnsRecoveryPersistsToSession(t *testing.T) {
	session := NewInMemorySession()
	tool := NewFunctionTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	opts := RunOptions{Conversation: ConversationOptions{Session: NewSession(session)}, Exec: ExecOptions{MaxTurns: 1, ErrorHandlers: RunErrorHandlers{
		MaxTurns: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{FinalOutput: "budget spent"}, nil
		},
	}}}
	if _, err := RunSync(context.Background(), agent, "go", opts); err != nil {
		t.Fatal(err)
	}
	items, err := session.ContextItems(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	// user input + turn-1 tool call + tool output + synthesized message
	if len(items) != 4 {
		t.Fatalf("session items = %d, want 4", len(items))
	}
	if items[len(items)-1].OfOutputMessage == nil {
		t.Fatalf("last session item is not the synthesized message: %+v", items[len(items)-1])
	}
}

func TestErrorHandlers_Streamed_EmitsSynthesizedMessage(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "not valid json"))}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{ErrorHandlers: RunErrorHandlers{
		InvalidFinalOutput: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{FinalOutput: sentiment{Label: "fallback", Score: 1}}, nil
		},
	}}}
	stream, _ := Run(context.Background(), agent, "hi", opts)
	var messageEvents []string
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if ie, ok := event.(*RunItemStreamEvent); ok && ie.Name == "message_output_created" {
			if m, ok := ie.Item.(*MessageOutputItem); ok {
				messageEvents = append(messageEvents, m.Text())
			}
		}
	}
	if got, _ := FinalOutputAs[sentiment](res); got.Label != "fallback" {
		t.Errorf("final output = %#v", res.FinalOutput)
	}
	if len(messageEvents) != 2 || messageEvents[1] != `{"label":"fallback","score":1}` {
		t.Errorf("message events = %q, want model message then synthesized fallback", messageEvents)
	}
}

func TestErrorHandlers_Streamed_MaxTurnsRecovery(t *testing.T) {
	tool := NewFunctionTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{MaxTurns: 1, ErrorHandlers: RunErrorHandlers{
		MaxTurns: func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error) {
			return &RunErrorHandlerResult{FinalOutput: "budget spent"}, nil
		},
	}}}
	stream, _ := Run(context.Background(), agent, "go", opts)
	sawSynthesized := false
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if ie, ok := event.(*RunItemStreamEvent); ok && ie.Name == "message_output_created" {
			if m, ok := ie.Item.(*MessageOutputItem); ok && m.Text() == "budget spent" {
				sawSynthesized = true
			}
		}
	}
	if res.FinalOutputString() != "budget spent" {
		t.Errorf("final output = %q", res.FinalOutputString())
	}
	if !sawSynthesized {
		t.Error("stream never emitted the synthesized fallback message")
	}
}
