package agents

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/responses"
)

// fakeModel is a scripted Model for testing the runner without a real API. Each
// call to GetResponse returns the next queued response.
type fakeModel struct {
	responses []*ModelResponse
	idx       int
	lastReq   ModelRequest
	calls     int
	// onRequest, when set, is called with each request as it arrives — for
	// tests that care about what the model was offered on a given turn, not
	// just on the last one.
	onRequest func(ModelRequest)
}

func (m *fakeModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.lastReq = req
	m.calls++
	if m.onRequest != nil {
		m.onRequest(req)
	}
	if m.idx >= len(m.responses) {
		return &ModelResponse{Output: nil, Usage: NewUsage()}, nil
	}
	resp := m.responses[m.idx]
	m.idx++
	if resp.Usage == nil {
		resp.Usage = &Usage{Requests: 1, TotalTokens: 1, InputTokens: 1}
	}
	return resp, nil
}

func (m *fakeModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.lastReq = req
		m.calls++
		if m.onRequest != nil {
			m.onRequest(req)
		}
		var resp *ModelResponse
		if m.idx < len(m.responses) {
			resp = m.responses[m.idx]
			m.idx++
		} else {
			resp = &ModelResponse{Usage: NewUsage()}
		}
		// Emit a single response.completed event carrying the queued response.
		event := completedStreamEvent(resp)
		yield(&event, nil)
	}
}

// completedStreamEvent builds a response.completed stream event whose embedded
// Response carries the given output items, so the streaming runner can assemble
// a ModelResponse from it.
func completedStreamEvent(resp *ModelResponse) TResponseStreamEvent {
	rawItems := make([]json.RawMessage, 0, len(resp.Output))
	for i := range resp.Output {
		rawItems = append(rawItems, json.RawMessage(resp.Output[i].RawJSON()))
	}
	outBytes, _ := json.Marshal(rawItems)
	payload := `{"type":"response.completed","sequence_number":0,"response":{"id":"resp_1","output":` +
		string(outBytes) + `,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,` +
		`"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`
	var event TResponseStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		panic(err)
	}
	return event
}

// --- output item builders (constructed via JSON so RawJSON is populated) ---

func mustOutputItem(t *testing.T, raw string) TResponseOutputItem {
	t.Helper()
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("build output item: %v", err)
	}
	return item
}

func messageOutput(t *testing.T, text string) TResponseOutputItem {
	t.Helper()
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":` +
		quote(text) + `,"annotations":[]}]}`
	return mustOutputItem(t, raw)
}

func functionCallOutput(t *testing.T, name, callID, args string) TResponseOutputItem {
	t.Helper()
	raw := `{"type":"function_call","id":"fc_1","call_id":` + quote(callID) +
		`,"name":` + quote(name) + `,"arguments":` + quote(args) + `,"status":"completed"}`
	return mustOutputItem(t, raw)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func modelResp(items ...TResponseOutputItem) *ModelResponse {
	return &ModelResponse{Output: items, Usage: &Usage{Requests: 1, InputTokens: 5, OutputTokens: 3, TotalTokens: 8}}
}

// --- tests ---

func TestRun_SingleTurnPlainText(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hello world"))}}
	agent := &Agent{Name: "assistant", Instructions: StaticInstructions("be nice"), ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "hello world" {
		t.Errorf("final output = %q", res.FinalOutputString())
	}
	if res.LastAgent != agent {
		t.Errorf("last agent mismatch")
	}
	if model.lastReq.SystemInstructions != "be nice" {
		t.Errorf("system instructions = %q", model.lastReq.SystemInstructions)
	}
	if res.Usage.TotalTokens != 8 {
		t.Errorf("usage total = %d, want 8", res.Usage.TotalTokens)
	}
}

func TestRun_ToolCallThenFinal(t *testing.T) {
	var toolCalled bool
	tool := NewTool("get_weather", "weather",
		func(ctx context.Context, tc *ToolContext, args struct {
			City string `json:"city"`
		}) (string, error) {
			toolCalled = true
			if args.City != "SF" {
				t.Errorf("city = %q", args.City)
			}
			return "sunny", nil
		})

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "call_1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "it is sunny")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "weather in SF?", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !toolCalled {
		t.Error("tool was not called")
	}
	if res.FinalOutputString() != "it is sunny" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2", model.calls)
	}
	// The second turn's input must include the tool call and its output.
	if len(model.lastReq.Input) < 3 {
		t.Errorf("second turn input too short: %d items", len(model.lastReq.Input))
	}
}

type sentiment struct {
	Label string `json:"label"`
	Score int    `json:"score"`
}

func TestRun_StructuredOutput(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, `{"label":"positive","score":9}`)),
	}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "analyze", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FinalOutputAs[sentiment](res)
	if !ok {
		t.Fatalf("final output type = %T", res.FinalOutput)
	}
	if got.Label != "positive" || got.Score != 9 {
		t.Errorf("got %+v", got)
	}
}

func TestRun_MaxTurnsExceeded(t *testing.T) {
	// Always returns a tool call, never a final output -> loops until max turns.
	tool := NewTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c3", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{Exec: ExecOptions{MaxTurns: 2}})
	if err == nil {
		t.Fatal("expected MaxTurnsError")
	}
	if CodeOf(err) != CodeMaxTurns {
		t.Errorf("error = %v, want CodeMaxTurns", err)
	}
	var mte *MaxTurnsError
	if !errors.As(err, &mte) {
		t.Errorf("error not a *MaxTurnsError: %T", err)
	}
}

// ShouldStopAfterTurn ends a run that would otherwise take another turn. It is
// what replaced the agent-level "stop at these tools" configuration: deciding
// from what the turn produced covers the same case and more.
func TestRun_ShouldStopAfterTurn(t *testing.T) {
	newAgent := func(model *fakeModel) *Agent {
		tool := NewTool("compute", "computes",
			func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
				return "the-answer", nil
			})
		return &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
	}

	t.Run("stops at a named tool, reporting its output", func(t *testing.T) {
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
			modelResp(messageOutput(t, "never reached")),
		}}
		res, err := RunSync(context.Background(), newAgent(model), "go", RunOptions{
			Exec: ExecOptions{ShouldStopAfterTurn: func(_ context.Context, tr *TurnResult) (bool, error) {
				return slices.Contains(tr.ToolCallNames(), "compute"), nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if model.calls != 1 {
			t.Errorf("model calls = %d, want 1 — the hook should have stopped the run", model.calls)
		}
		// No message this turn, so the run reports the tool output rather than
		// an empty final output.
		if got := res.FinalOutputString(); got != "the-answer" {
			t.Errorf("final = %q, want the-answer", got)
		}
	})

	t.Run("returning false leaves the run alone", func(t *testing.T) {
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
			modelResp(messageOutput(t, "all done")),
		}}
		var turns []int
		res, err := RunSync(context.Background(), newAgent(model), "go", RunOptions{
			Exec: ExecOptions{ShouldStopAfterTurn: func(_ context.Context, tr *TurnResult) (bool, error) {
				turns = append(turns, tr.Turn)
				return false, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalOutputString() != "all done" {
			t.Errorf("final = %q", res.FinalOutputString())
		}
		// Only the tool turn asks: the final turn ends the run on its own, and
		// asking whether to stop a run that is already stopping is noise.
		if !slices.Equal(turns, []int{1}) {
			t.Errorf("hook saw turns %v, want [1]", turns)
		}
	})

	t.Run("an error from the hook fails the run", func(t *testing.T) {
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
		}}
		_, err := RunSync(context.Background(), newAgent(model), "go", RunOptions{
			Exec: ExecOptions{ShouldStopAfterTurn: func(context.Context, *TurnResult) (bool, error) {
				return false, errors.New("boom")
			}},
		})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want the hook's error", err)
		}
	})

	t.Run("stops at a handoff before control leaves the agent", func(t *testing.T) {
		billing := &Agent{Name: "billing", ModelImpl: &fakeModel{
			responses: []*ModelResponse{modelResp(messageOutput(t, "never reached"))},
		}}
		triageModel := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "transfer_to_billing", "c1", `{}`)),
		}}
		triage := &Agent{Name: "triage", ModelImpl: triageModel, Handoffs: []Handoff{HandoffTo(billing)}}

		res, err := RunSync(context.Background(), triage, "go", RunOptions{
			Exec: ExecOptions{ShouldStopAfterTurn: func(context.Context, *TurnResult) (bool, error) {
				return true, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.LastAgent != triage {
			t.Errorf("last agent = %s, want triage — the handoff should not have been taken", res.LastAgent.Name)
		}
	})
}

func TestRun_UnknownToolErrors(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "ghost", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Errorf("error not *ModelBehaviorError: %T (%v)", err, err)
	}
}

func TestRun_Handoff(t *testing.T) {
	billing := &Agent{Name: "billing"}
	billingModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "billing handled it"))}}
	billing.ModelImpl = billingModel

	triageModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "transfer_to_billing", "c1", `{}`)),
	}}
	triage := &Agent{
		Name:      "triage",
		ModelImpl: triageModel,
		Handoffs:  []Handoff{HandoffTo(billing)},
	}

	res, err := RunSync(context.Background(), triage, "I have a billing question", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.LastAgent != billing {
		t.Errorf("last agent = %v, want billing", res.LastAgent.Name)
	}
	if res.FinalOutputString() != "billing handled it" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func TestRun_ParallelTools(t *testing.T) {
	var aCalled, bCalled bool
	toolA := NewTool("tool_a", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		aCalled = true
		return "a-done", nil
	})
	toolB := NewTool("tool_b", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		bCalled = true
		return "b-done", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{
			functionCallOutput(t, "tool_a", "c1", `{}`),
			functionCallOutput(t, "tool_b", "c2", `{}`),
		}, Usage: NewUsage()},
		modelResp(messageOutput(t, "both done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{toolA, toolB}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "do both", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !aCalled || !bCalled {
		t.Errorf("tools called: a=%v b=%v", aCalled, bCalled)
	}
	if res.FinalOutputString() != "both done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func TestRun_IsEnabledHidesTool(t *testing.T) {
	tool := NewTool("secret", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		return "", nil
	})
	tool.IsEnabled = func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error) {
		return false, nil
	}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(model.lastReq.Tools) != 0 {
		t.Errorf("disabled tool should be hidden, got %d tools", len(model.lastReq.Tools))
	}
}

// MaxTurnsUnlimited disables the turn budget — a run that would exceed the
// default of 10 turns completes.
func TestRun_MaxTurnsUnlimited(t *testing.T) {
	tool := NewTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	// 15 tool-call turns (well past the default 10) then a final message.
	var responses []*ModelResponse
	for range 15 {
		responses = append(responses, modelResp(functionCallOutput(t, "loop", "c", `{}`)))
	}
	responses = append(responses, modelResp(messageOutput(t, "finally done")))
	model := &fakeModel{responses: responses}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{Exec: ExecOptions{MaxTurns: MaxTurnsUnlimited}})
	if err != nil {
		t.Fatalf("unlimited run should not hit a turn cap: %v", err)
	}
	if res.FinalOutputString() != "finally done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// On resume the interrupted run's budget wins; a small opts.Exec.MaxTurns does
// not shrink it.
func TestRun_ResumeIgnoresOptsMaxTurns(t *testing.T) {
	tool := NewTool("act", "acts",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "act", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{Exec: ExecOptions{MaxTurns: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State == nil || res.State.MaxTurns != 5 {
		t.Fatalf("state MaxTurns = %v, want 5", res.State)
	}
	res.State.Approve(res.Interruptions[0], false)
	// A tiny opts.Exec.MaxTurns must be ignored — the state's budget of 5 governs.
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{Exec: ExecOptions{MaxTurns: 1}})
	if err != nil {
		t.Fatalf("resume should run under the state's budget, not opts: %v", err)
	}
	if res2.FinalOutputString() != "done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}

// blockingModel blocks in GetResponse until its context is cancelled, then
// reports the cancellation. Used to prove an input-guardrail tripwire cancels
// the in-flight model call.
type blockingModel struct {
	called    chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (m *blockingModel) GetResponse(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.once.Do(func() { close(m.called) })
	<-ctx.Done()
	close(m.cancelled)
	return nil, ctx.Err()
}

func (m *blockingModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(func(*TResponseStreamEvent, error) bool) {}
}

// An input-guardrail tripwire cancels the in-flight model call rather than
// waiting for a response nobody will use.
func TestRun_InputGuardrailTripwireCancelsModel(t *testing.T) {
	model := &blockingModel{called: make(chan struct{}), cancelled: make(chan struct{})}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "trip",
			Stages: []GuardrailStage{StageInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				<-model.called // ensure the model call is in flight first
				return Trip(nil), nil
			},
		}},
	}

	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	var tw *GuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("err = %v, want *GuardrailTripwireError", err)
	}
	select {
	case <-model.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("model call was not cancelled by the tripwire")
	}
}
