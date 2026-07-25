package agents

import (
	"context"
	"errors"
	"testing"
	"time"
)

func baseTool(t *testing.T) Tool {
	t.Helper()
	return NewFunctionTool("probe", "probe",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "ok", nil })
}

// THE constraint this design exists for. A bare type assertion compiles and
// silently returns false through a decorator — embedding the Tool interface
// promotes Tool's own methods and nothing else. ToolAs walks the chain.
func TestToolAs_SeesThroughStackedDecorators(t *testing.T) {
	base := baseTool(t)
	stacked := WithTimeout(WithGuardrails(WithApprovalAlways(base), Guardrail{Name: "g"}), time.Second)

	// The bare assertion that would look right and be wrong:
	if _, ok := stacked.(ApprovalRequiredTool); ok {
		t.Error("a bare assertion now succeeds; the ToolAs requirement may have changed")
	}

	if _, ok := ToolAs[ApprovalRequiredTool](stacked); !ok {
		t.Error("approval is invisible through the stack")
	}
	if g, ok := ToolAs[GuardedTool](stacked); !ok || len(g.ToolGuardrails()) == 0 {
		t.Error("guardrails are invisible through the stack")
	}
	if tt, ok := ToolAs[TimeoutTool](stacked); !ok || tt.ToolTimeout() != time.Second {
		t.Error("the timeout is invisible through the stack")
	}
	if _, ok := ToolAs[InvokableTool](stacked); !ok {
		t.Error("the tool underneath is no longer invokable")
	}
	if stacked.ToolName() != "probe" {
		t.Errorf("name = %q, want it forwarded", stacked.ToolName())
	}
}

// Order must not matter: whichever way they are stacked, every capability is
// still findable.
func TestToolAs_OrderIndependent(t *testing.T) {
	orders := map[string]Tool{
		"approval outermost": WithApprovalAlways(WithTimeout(baseTool(t), time.Second)),
		"timeout outermost":  WithTimeout(WithApprovalAlways(baseTool(t)), time.Second),
		"three deep":         WithSequential(WithTimeout(WithApprovalAlways(baseTool(t)), time.Second)),
	}
	for name, tool := range orders {
		t.Run(name, func(t *testing.T) {
			if _, ok := ToolAs[ApprovalRequiredTool](tool); !ok {
				t.Error("approval not found")
			}
			if _, ok := ToolAs[TimeoutTool](tool); !ok {
				t.Error("timeout not found")
			}
			if _, ok := ToolAs[InvokableTool](tool); !ok {
				t.Error("invoker not found")
			}
		})
	}
}

// The runner asks through ToolAs, so a decorated tool behaves like one built
// with the equivalent fields.
func TestDecorators_TakeEffectInARun(t *testing.T) {
	t.Run("approval interrupts", func(t *testing.T) {
		tool := WithApprovalAlways(baseTool(t))
		agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}}
		res, err := RunSync(context.Background(), agent, "go", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Interruptions) != 1 {
			t.Fatalf("interruptions = %d, want 1 from the decorator", len(res.Interruptions))
		}
	})

	t.Run("timeout applies", func(t *testing.T) {
		slow := NewFunctionTool("slow", "slow",
			func(ctx context.Context, _ *ToolContext, _ struct{}) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			})
		slow.FailureErrorFunction = nil // make the timeout fatal so the test sees it
		tool := WithTimeout(slow, 20*time.Millisecond)
		agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "slow", "c1", `{}`)),
		}}}
		_, err := RunSync(context.Background(), agent, "go", RunOptions{})
		var te *ToolTimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v, want *ToolTimeoutError from the decorator", err)
		}
	})

	t.Run("guardrails apply", func(t *testing.T) {
		tool := WithGuardrails(baseTool(t), Guardrail{
			Name:   "rewrite",
			Stages: []GuardrailStage{StageToolOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Replace("substituted", nil), nil
			},
		})
		agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}}
		res, err := RunSync(context.Background(), agent, "go", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out := findToolOutput(res.NewItems)
		if out == nil || stringifyToolOutput(out.Output) != "substituted" {
			t.Errorf("tool output = %v, want the decorator's guardrail to apply", out)
		}
	})

	t.Run("disabled hides the tool", func(t *testing.T) {
		tool := WithEnabled(baseTool(t), func(context.Context, *RunContext, *Agent) (bool, error) {
			return false, nil
		})
		model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}}
		agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}
		if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err != nil {
			t.Fatal(err)
		}
		if len(model.lastReq.Tools) != 0 {
			t.Errorf("the model was offered %d tools, want none", len(model.lastReq.Tools))
		}
	})
}

// Wrapping a tool that already has guardrails must ADD to them, not replace
// them: silently dropping an inner tool's safety checks would be a trap.
func TestWithGuardrails_AddsRatherThanReplaces(t *testing.T) {
	inner := NewFunctionTool("probe", "probe",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "ok", nil })
	inner.Guardrails = []Guardrail{{Name: "inner", Stages: []GuardrailStage{StageToolInput}}}

	wrapped := WithGuardrails(inner, Guardrail{Name: "outer", Stages: []GuardrailStage{StageToolInput}})
	g, ok := ToolAs[GuardedTool](wrapped)
	if !ok {
		t.Fatal("guardrails not found")
	}
	names := map[string]bool{}
	for _, gr := range g.ToolGuardrails() {
		names[gr.Name] = true
	}
	if !names["inner"] || !names["outer"] {
		t.Errorf("guardrails = %v, want both the inner tool's and the wrapper's", names)
	}
}

// A decorated tool still reaches the model with the name and schema of the tool
// underneath it — wrapping must not change what the model is told.
func TestDecorators_DoNotChangeWhatTheModelSees(t *testing.T) {
	tool := WithTimeout(WithApprovalAlways(baseTool(t)), time.Second)
	d, ok := ToolAs[DescribableTool](tool)
	if !ok {
		t.Fatal("a decorated tool cannot describe itself; the model would not be told about it")
	}
	if d.ToolDescription() != "probe" {
		t.Errorf("description = %q, want the inner tool's", d.ToolDescription())
	}
	if d.ToolParamsSchema() == nil {
		t.Error("schema lost through decoration")
	}
}
