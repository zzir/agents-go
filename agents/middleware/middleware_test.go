package middleware

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// The point of Loop: "the model stopped talking" and "the answer is good
// enough" are different questions, and the second one is the caller's.
func TestLoop_RunsAgainUntilTheEvaluatorAccepts(t *testing.T) {
	agent := says(t, "first draft", "second draft", "third draft")
	var seen []string

	res, err := agents.RunSync(context.Background(), agent, "write it", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Loop{
			Evaluate: func(_ context.Context, r *agents.RunResult) (Evaluation, error) {
				seen = append(seen, r.FinalOutputString())
				if strings.HasPrefix(r.FinalOutputString(), "third") {
					return Stop(), nil
				}
				return Continue("not good enough, try again"), nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "third draft" {
		t.Errorf("final = %q, want the accepted attempt", res.FinalOutputString())
	}
	if len(seen) != 3 {
		t.Errorf("evaluator saw %d attempts (%v), want 3", len(seen), seen)
	}
}

// An evaluator that never accepts must not run forever on the caller's budget.
func TestLoop_BoundedByMaxAttempts(t *testing.T) {
	agent := says(t, "a", "b", "c", "d", "e")
	attempts := 0

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Loop{
			MaxAttempts: 2,
			Evaluate: func(context.Context, *agents.RunResult) (Evaluation, error) {
				attempts++
				return Continue("again"), nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("ran %d attempts, want 2", attempts)
	}
	// The last attempt is still the run's result: a bounded loop reports what
	// it got, it does not fail.
	if res.FinalOutputString() != "b" {
		t.Errorf("final = %q, want the last attempt", res.FinalOutputString())
	}
}

// A rejected attempt is carried forward, or the agent just says it again.
func TestLoop_FeedsTheAttemptBackIn(t *testing.T) {
	model := &scriptedModel{responses: []*agents.ModelResponse{resp(message(t, "one")), resp(message(t, "two"))}}
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	var inputs [][]agents.InputItem
	if _, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{
			Loop{MaxAttempts: 2, Evaluate: func(context.Context, *agents.RunResult) (Evaluation, error) {
				return Continue("say it differently"), nil
			}},
			// Inner to the loop, so it sees what each attempt was given.
			agents.RunMiddlewareFunc(func(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
				inputs = append(inputs, in.Input)
				return next(ctx, in)
			}),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 {
		t.Fatalf("saw %d attempts, want 2", len(inputs))
	}
	if len(inputs[1]) <= len(inputs[0]) {
		t.Errorf("the second attempt got %d items, the first %d — the rejected answer and the feedback are missing",
			len(inputs[1]), len(inputs[0]))
	}
}

// A standing rule should not have to be re-litigated by every caller driving a
// run: the policy answers on the SDK side of the pause.
func TestApproval_PolicyResolvesTheInterruption(t *testing.T) {
	tool := agents.NewTool("read_file", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "contents", nil
	})
	tool.NeedsApproval = true
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{tool}, ModelImpl: &scriptedModel{
		responses: []*agents.ModelResponse{
			resp(toolCall(t, "read_file", "c1")),
			resp(message(t, "done")),
		}}}

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Approval{Policy: AllowTools("read_file")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 0 {
		t.Errorf("the run still paused on %d interruptions", len(res.Interruptions))
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q, want the resumed run's answer", res.FinalOutputString())
	}
}

// A policy is a shortcut for what a human already decided, not a replacement
// for asking: anything it does not recognize still reaches the caller.
func TestApproval_UnrecognizedCallsStillPause(t *testing.T) {
	tool := agents.NewTool("rm", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "gone", nil
	})
	tool.NeedsApproval = true
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{tool}, ModelImpl: &scriptedModel{
		responses: []*agents.ModelResponse{resp(toolCall(t, "rm", "c1"))}}}

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Approval{Policy: AllowTools("read_file")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want the unrecognized call to reach the caller", len(res.Interruptions))
	}
}

func TestApproval_DenyFeedsTheReasonBack(t *testing.T) {
	tool := agents.NewTool("rm", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		t.Error("a denied tool executed")
		return "", nil
	})
	tool.NeedsApproval = true
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{tool}, ModelImpl: &scriptedModel{
		responses: []*agents.ModelResponse{
			resp(toolCall(t, "rm", "c1")),
			resp(message(t, "understood")),
		}}}

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Approval{
			Policy: func(context.Context, *agents.ToolApprovalItem) (Decision, string) {
				return Deny, "not allowed here"
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "understood" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// Retry covers what the loop could not absorb; NewRetryModel covers one call.
func TestRetry_RerunsAFailedRun(t *testing.T) {
	// The first attempt calls a tool the agent does not expose, which fails the
	// run; the second answers normally.
	model := &scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCall(t, "ghost", "c1")),
		resp(message(t, "recovered")),
	}}
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Retry{MaxAttempts: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2 (one per attempt)", model.calls)
	}
}

func TestRetry_GivesUpAndReportsTheError(t *testing.T) {
	model := &scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCall(t, "ghost", "c1")),
		resp(toolCall(t, "ghost", "c2")),
	}}
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	_, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Retry{MaxAttempts: 2}},
	})
	if _, ok := errors.AsType[*agents.ModelBehaviorError](err); !ok {
		t.Errorf("err = %v, want the original failure after the retries ran out", err)
	}
}

// A cancelled context is the caller saying stop, not a failure to retry.
func TestRetry_DoesNotRetryACancelledRun(t *testing.T) {
	model := &scriptedModel{responses: []*agents.ModelResponse{resp(message(t, "never"))}}
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Retry{MaxAttempts: 5}},
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if model.calls > 1 {
		t.Errorf("model called %d times on a cancelled run", model.calls)
	}
}

// capture records what each attempt was given, from inside the chain.
func capture(inputs *[][]agents.InputItem) agents.RunMiddleware {
	return agents.RunMiddlewareFunc(func(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
		*inputs = append(*inputs, in.Input)
		return next(ctx, in)
	})
}

// userTexts reads the user-authored items stored in a session, in order.
func userTexts(t *testing.T, sess *session.Session) []string {
	t.Helper()
	entries, err := sess.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Kind != session.EntryKindItem || e.Source.Type != agents.SourceUser {
			continue
		}
		item, err := session.UnmarshalInputItem(e.Item)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, session.ItemText(item))
	}
	return out
}

// With a session the attempt is already in the history, so only the feedback
// goes to the next attempt — re-sending the prior turns would store and send
// them twice (spec §2.12).
func TestLoop_WithASessionCarriesOnlyTheFeedback(t *testing.T) {
	sess := session.NewInMemorySession()
	agent := says(t, "one", "two")
	var inputs [][]agents.InputItem

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Middlewares: []agents.RunMiddleware{
			Loop{MaxAttempts: 2, Evaluate: func(context.Context, *agents.RunResult) (Evaluation, error) {
				return Continue("again"), nil
			}},
			capture(&inputs),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "two" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if len(inputs) != 2 || len(inputs[1]) != 1 || session.ItemText(inputs[1][0]) != "again" {
		t.Errorf("second attempt's input = %v, want the feedback alone", inputs)
	}
	if got := userTexts(t, sess); !slices.Equal(got, []string{"go", "again"}) {
		t.Errorf("stored user input = %v, want [go again]", got)
	}
}

// flakyModel fails its first calls, then answers the script; it records every
// request's input.
type flakyModel struct {
	scriptedModel
	failures int
	inputs   [][]agents.InputItem
}

func (m *flakyModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	m.inputs = append(m.inputs, req.Input)
	if m.failures > 0 {
		m.failures--
		return nil, errors.New("model unavailable")
	}
	return m.scriptedModel.Respond(ctx, req)
}

// Once an attempt has stored the input, a retry sends none: the session holds
// it, and sending it again would store and send it twice (spec §2.12).
func TestRetry_WithASessionDoesNotResendStoredInput(t *testing.T) {
	sess := session.NewInMemorySession()
	model := &flakyModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{resp(message(t, "ok"))}}, failures: 1}
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	var inputs [][]agents.InputItem

	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Middlewares:  []agents.RunMiddleware{Retry{MaxAttempts: 2}, capture(&inputs)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "ok" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if len(inputs) != 2 || len(inputs[1]) != 0 {
		t.Errorf("retry input = %v, want none", inputs)
	}
	if got := userTexts(t, sess); !slices.Equal(got, []string{"go"}) {
		t.Errorf("stored user input = %v, want [go]", got)
	}
	// The model still sees the prompt on the retry — from the history.
	if len(model.inputs) != 2 || len(model.inputs[1]) != 1 || session.ItemText(model.inputs[1][0]) != "go" {
		t.Errorf("retry model input = %v, want the stored prompt once", model.inputs)
	}
}

// Without a session nothing stored the input, so the retry sends it again.
func TestRetry_WithoutASessionResendsTheInput(t *testing.T) {
	model := &flakyModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{resp(message(t, "ok"))}}, failures: 1}
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	var inputs [][]agents.InputItem

	if _, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Retry{MaxAttempts: 2}, capture(&inputs)},
	}); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || len(inputs[1]) != 1 || session.ItemText(inputs[1][0]) != "go" {
		t.Errorf("retry input = %v, want the original prompt", inputs)
	}
}

// A policy resume continues under the caller's own control: a StopAfterTurn
// asked for after the resume still lands (spec §2.12).
func TestApproval_ResumeKeepsTheCallersControl(t *testing.T) {
	var ctrl agents.RunControl
	gated := agents.NewTool("gated", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "done", nil
	})
	gated.NeedsApproval = true
	stopper := agents.NewTool("stopper", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		ctrl.StopAfterTurn()
		return "stopping", nil
	})
	model := &scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCall(t, "gated", "c1")),
		resp(toolCall(t, "stopper", "c2")),
		resp(message(t, "never")),
	}}
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{gated, stopper}, ModelImpl: model}

	stream, c := agents.Run(context.Background(), agent, "go", agents.RunOptions{
		Middlewares: []agents.RunMiddleware{Approval{Policy: AllowTools("gated")}},
	})
	ctrl = c
	res, err := stream.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if !res.StoppedEarly {
		t.Error("the stop asked for on the caller's control was lost across the policy resume")
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2 (the run should stop before the third)", model.calls)
	}
}
