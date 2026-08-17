package bridge

import (
	"cmp"
	"context"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

// How a run is configured: the SDK RunOptions a build turns into, the session
// wrapped for compaction, and the small policies read off the config.

// compactionNotifier drives the chat UI's live indicator with transient
// run.compaction status events. Trace recording is the compaction span's job
// (opened by the SDK runner via CompactionArgs.StartSpan), not the notifier's.
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

// runOptionsFor assembles the RunOptions shared by the fresh-run and resume
// paths. One constructor so a resume carries the same policies as the run it
// continues. runContext is the Context value (the exec_command approval gate
// reads a trusted session id from it).
func runOptionsFor(built *BuildResult, sess *session.Session, provider agents.ModelProvider, tracer *tracing.Tracer, runContext any) agents.RunOptions {
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
			// Context overflow → forced compaction pass → retry the turn. Only
			// bites when the session is compaction-aware; otherwise the overflow
			// reports as before (spec §2.5g).
			Overflow: agents.OverflowPolicy{MaxRetries: 2},
		},
		Guardrails: built.RunGuardrails,
		Model:      agents.ModelOptions{Provider: provider},
		Observe:    agents.ObserveOptions{Tracer: tracer, IncludeSensitiveData: built.TraceIncludeSensitive},
	}
	if built.Behavior.HandoffInputFilter == "nest_history" {
		opts.Exec.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	return opts
}

// toolNotFoundBehavior resolves the agent's setting. Unset means RETURN TO
// MODEL here, not the SDK's stricter default: a model inventing a tool name —
// or reaching for one plan mode is hiding, or one a session without a sandbox
// never had — is a routine slip, and ending the run over it takes down the
// turn, and any workflow driving it, for something the model corrects on being
// told. Set "error" to get the abort back.
func toolNotFoundBehavior(s string) agents.ToolNotFoundBehavior {
	if s == "" {
		return agents.ToolNotFoundReturnToModel
	}
	return agents.ParseToolNotFoundBehavior(s)
}

// wrapCompaction wraps sa with the compaction adapter when the agent config
// enables it. An empty summary model falls back to the agent's own model, so
// leaving the field blank does not silently disable compaction.
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

// summaryModelFor resolves the model a compaction pass summarizes with — the
// config's compaction_model, else the agent's own. One definition, shared by
// the run path and the manual CompactSession.
func summaryModelFor(provider agents.ModelProvider, compaction store.CompactionGroup, agentModel string) (agents.Model, error) {
	return provider.Model(cmp.Or(compaction.Model, agentModel))
}

// stopAtTools builds the turn hook for the agent config's stop_at_tools list:
// the run ends after a turn that called any of the named tools. It returns nil
// for an empty list so an unconfigured agent pays nothing.
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
