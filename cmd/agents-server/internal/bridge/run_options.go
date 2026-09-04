package bridge

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

// How a run is configured: the SDK RunOptions a build turns into, the session
// wrapped for compaction, and the small policies read off the config.

// compactionNotifier drives the chat UI's live indicator with run.compaction
// events; the trace span is the SDK runner's (CompactionArgs.StartSpan).
func compactionNotifier(send func(string, any), runID string) store.CompactionNotifier {
	return store.CompactionNotifier{
		OnStart: func() {
			send(protocol.EventRunCompaction, protocol.RunCompaction{RunID: runID, Phase: "started"})
		},
		OnDone: func(before, after int) {
			send(protocol.EventRunCompaction, protocol.RunCompaction{
				RunID:  runID,
				Phase:  "finished",
				Detail: fmt.Sprintf("compacted %d→%d items", before, after),
			})
		},
	}
}

// runOptionsFor assembles the RunOptions shared by fresh and resume paths;
// runContext is the Context value the exec_command gate reads a session id from.
func runOptionsFor(built *BuildResult, sess *session.Session, provider agents.ModelProvider, tracer *tracing.Tracer, runContext any, log *slog.Logger) agents.RunOptions {
	opts := agents.RunOptions{
		Context: runContext,
		Conversation: agents.ConversationOptions{
			Session: sess,
			// A non-positive HistoryLimit already means "no limit" on both
			// sides, so it needs no translation.
			Settings: session.Settings{Limit: built.Session.HistoryLimit},
		},
		Exec: agents.ExecOptions{
			MaxTurns:              built.Behavior.MaxTurns,
			MaxToolConcurrency:    built.Behavior.MaxToolConcurrency,
			ErrorHandlers:         built.ErrorHandlers,
			ReasoningItemIDPolicy: built.ReasoningItemIDPolicy,
			ToolNotFoundBehavior:  toolNotFoundBehavior(built.Behavior.ToolNotFoundBehavior),
			ShouldStopAfterTurn:   stopAtTools(built.StopAtTools),
			// Context overflow → forced compaction → retry, only on a
			// compaction-aware session (spec §2.5g).
			Overflow: agents.OverflowPolicy{MaxRetries: 2},
		},
		Guardrails: built.RunGuardrails,
		Model:      agents.ModelOptions{Provider: provider},
		Observe:    agents.ObserveOptions{Tracer: tracer, IncludeSensitiveData: &built.TraceIncludeSensitive},
		// The run loop's own records join the server's stream. Most of what it
		// says is Debug, so this shows only at --log-level debug.
		Log: agents.LogConfig{Logger: log, SensitiveData: built.LogSensitive},
	}
	if built.Behavior.HandoffInputFilter == "nest_history" {
		opts.Exec.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	return opts
}

// toolNotFoundBehavior: unset means RETURN TO MODEL, not the SDK's stricter
// default — a model inventing a tool name is a routine slip; "error" aborts.
func toolNotFoundBehavior(s string) agents.ToolNotFoundBehavior {
	if s == "" {
		return agents.ToolNotFoundReturnToModel
	}
	return agents.ParseToolNotFoundBehavior(s)
}

// wrapCompaction wraps sa with the compaction adapter when the config enables
// it; an empty summary model falls back to the agent's own.
func wrapCompaction(sa *store.EntryStore, built *BuildResult, provider agents.ModelProvider, send func(string, any), runID string) *session.Session {
	if !built.Compaction.Enabled || provider == nil {
		return session.NewSession(sa)
	}
	summaryModel, err := summaryModelFor(provider, built.Compaction, built.Agent.Model)
	if err != nil || summaryModel == nil {
		return session.NewSession(sa)
	}
	return session.NewSession(store.NewCompactionAdapter(sa, summaryModel,
		built.Compaction.Threshold, built.Compaction.Window, built.Compaction.Prompt,
		compactionNotifier(send, runID),
	))
}

// summaryModelFor resolves the compaction summary model — compaction_model,
// else the agent's own; shared by the run path and CompactSession.
func summaryModelFor(provider agents.ModelProvider, compaction store.CompactionGroup, agentModel string) (agents.Model, error) {
	return provider.Model(cmp.Or(compaction.Model, agentModel))
}

// stopAtTools builds the turn hook for stop_at_tools; nil for an empty list.
func stopAtTools(names []string) func(context.Context, *agents.TurnResult) (bool, error) {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	return func(_ context.Context, tr *agents.TurnResult) (bool, error) {
		for _, called := range tr.ToolCallNames() {
			if want[called] {
				return true, nil
			}
		}
		return false, nil
	}
}
