package agents_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
	"github.com/zzir/agents-go/tracing"
)

// closeRecorder logs a trace's close into the order a test asserts.
type closeRecorder struct{ order *[]string }

func (r closeRecorder) OnTraceEnd(*tracing.Trace) { *r.order = append(*r.order, "trace closed") }
func (closeRecorder) OnTraceStart(*tracing.Trace) {}
func (closeRecorder) OnSpanStart(*tracing.Span)   {}
func (closeRecorder) OnSpanEnd(*tracing.Span)     {}
func (closeRecorder) ForceFlush()                 {}
func (closeRecorder) Shutdown(context.Context)    {}

// The trace closes before the stream's last yield, for a fresh run and a
// resumed one alike, and closes all the same when the consumer leaves first
// (spec §2.0).
func TestTraceClosesBeforeTheLastYield(t *testing.T) {
	ctx := context.Background()
	gated := agents.NewTool("gated", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "done", nil
	})
	gated.NeedsApproval = true
	// start returns the stream under test: a fresh run, or the resume of one
	// paused untraced at the gated call. Its last turn answers or fails.
	start := func(t *testing.T, resume, fail bool, opts agents.RunOptions) agents.RunStream {
		t.Helper()
		script := agentstest.NewResponseBuilder()
		if resume {
			script.FunctionCall("gated", "call-1", "{}").NewTurn()
		}
		if fail {
			script.Fail(errors.New("model down"))
		} else {
			script.Text("hi")
		}
		agent := &agents.Agent{Name: "a", ModelImpl: script.Build(), Tools: []*agents.Tool{gated}}
		if !resume {
			stream, _ := agents.Run(ctx, agent, "go", opts)
			return stream
		}
		paused, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(paused.Interruptions) != 1 {
			t.Fatalf("interruptions = %d, want the gated call", len(paused.Interruptions))
		}
		paused.State.Approve(paused.Interruptions[0], false)
		stream, _ := agents.ResumeRun(ctx, paused.State, opts)
		return stream
	}

	for _, tc := range []struct {
		name                  string
		resume, fail, abandon bool
		want                  []string
	}{
		{name: "Run completes", want: []string{"trace closed", "completed"}},
		{name: "Run fails", fail: true, want: []string{"trace closed", "failed"}},
		{name: "Run is abandoned", abandon: true, want: []string{"trace closed"}},
		{name: "ResumeRun completes", resume: true, want: []string{"trace closed", "completed"}},
		{name: "ResumeRun fails", resume: true, fail: true, want: []string{"trace closed", "failed"}},
		{name: "ResumeRun is abandoned", resume: true, abandon: true, want: []string{"trace closed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			stream := start(t, tc.resume, tc.fail, agents.RunOptions{
				Observe: agents.ObserveOptions{Tracer: tracing.NewTracer(closeRecorder{&order})},
			})
			for ev, err := range stream {
				if tc.abandon {
					break
				}
				if err != nil {
					order = append(order, "failed")
				}
				if _, ok := ev.(*agents.RunCompletedEvent); ok {
					order = append(order, "completed")
				}
			}
			if !slices.Equal(order, tc.want) {
				t.Fatalf("order = %v, want %v", order, tc.want)
			}
		})
	}
}
