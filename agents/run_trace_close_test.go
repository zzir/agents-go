package agents_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
	"github.com/zzir/agents-go/tracing"
)

// traceCounter counts trace lifecycle notifications.
type traceCounter struct{ started, ended atomic.Int32 }

func (c *traceCounter) OnTraceStart(*tracing.Trace) { c.started.Add(1) }
func (c *traceCounter) OnTraceEnd(*tracing.Trace)   { c.ended.Add(1) }
func (*traceCounter) OnSpanStart(*tracing.Span)     {}
func (*traceCounter) OnSpanEnd(*tracing.Span)       {}

// unreadableStorage fails every history read.
type unreadableStorage struct{ session.Storage }

func (unreadableStorage) Entries(context.Context, session.Cursor) ([]session.Entry, error) {
	return nil, errors.New("storage offline")
}

// A run that fails while loading history has already opened its trace, and a
// trace always closes.
func TestTraceClosesWhenHistoryFailsToLoad(t *testing.T) {
	proc := &traceCounter{}
	agent := &agents.Agent{Name: "a", ModelImpl: agentstest.TextModel("hi")}
	_, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{
		Conversation: agents.ConversationOptions{
			Session: session.NewSession(unreadableStorage{session.NewInMemoryStorage("s")}),
		},
		Observe: agents.ObserveOptions{Tracer: tracing.NewTracer(proc)},
	})
	if err == nil {
		t.Fatal("want the history read's error")
	}
	if s, e := proc.started.Load(), proc.ended.Load(); s != 1 || e != 1 {
		t.Fatalf("trace started %d times and ended %d, want 1 and 1", s, e)
	}
}
