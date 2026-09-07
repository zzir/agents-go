package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// runOptionsFor is the single constructor both the fresh-run and resume paths
// use — that sharing IS the regression guard (the resume path once dropped
// HandoffInputFilter and ToolNotFoundBehavior by re-listing fields by hand).
// This pins that every configured policy actually lands in the options.
func TestRunOptionsForCarriesEveryPolicy(t *testing.T) {
	built := &BuildResult{
		Behavior: store.BehaviorGroup{
			MaxTurns:             7,
			MaxToolConcurrency:   3,
			HandoffInputFilter:   "nest_history",
			ToolNotFoundBehavior: "continue",
		},
		Session:       store.SessionGroup{HistoryLimit: 25},
		StopAtTools:   []string{"final_answer"},
		RunGuardrails: []agents.Guardrail{{Name: "g1"}},
	}
	opts := runOptionsFor(built, nil, nil, nil, "trust-ctx", nil, agents.ContextBudget{})

	if opts.Exec.MaxTurns != 7 || opts.Exec.MaxToolConcurrency != 3 {
		t.Fatalf("exec budgets not carried: %+v", opts.Exec)
	}
	if opts.Exec.HandoffInputFilter == nil {
		t.Fatal("nest_history must set Exec.HandoffInputFilter")
	}
	if opts.Exec.ToolNotFoundBehavior != agents.ParseToolNotFoundBehavior("continue") {
		t.Fatalf("ToolNotFoundBehavior not carried: %v", opts.Exec.ToolNotFoundBehavior)
	}
	if opts.Exec.ShouldStopAfterTurn == nil {
		t.Fatal("StopAtTools must set Exec.ShouldStopAfterTurn")
	}
	if opts.Conversation.Settings.Limit != 25 {
		t.Fatalf("HistoryLimit not carried: Conversation.Settings = %+v", opts.Conversation.Settings)
	}
	if len(opts.Guardrails) != 1 || opts.Guardrails[0].Name != "g1" {
		t.Fatalf("run guardrails not carried: %+v", opts.Guardrails)
	}
	if opts.Context != "trust-ctx" {
		t.Fatalf("run context not carried: %v", opts.Context)
	}
	if opts.Exec.Overflow.MaxRetries <= 0 {
		t.Fatal("overflow recovery must be on — compaction-aware sessions rely on it")
	}

	// Without nest_history the filter stays nil — the SDK default.
	built.Behavior.HandoffInputFilter = ""
	if got := runOptionsFor(built, nil, nil, nil, "", nil, agents.ContextBudget{}); got.Exec.HandoffInputFilter != nil {
		t.Fatal("empty HandoffInputFilter must leave the SDK default")
	}
}

// An unconfigured agent feeds a bad tool name BACK to the model instead of
// ending the run: models invent tool names, and plan mode hides real ones, so
// aborting would take down the turn and any workflow driving it over a slip
// the model corrects on being told. "error" restores the abort.
func TestUnknownToolReturnsToTheModelByDefault(t *testing.T) {
	built := &BuildResult{}
	if got := runOptionsFor(built, nil, nil, nil, "", nil, agents.ContextBudget{}); got.Exec.ToolNotFoundBehavior != agents.ToolNotFoundReturnToModel {
		t.Fatalf("unset behavior = %v, want return-to-model", got.Exec.ToolNotFoundBehavior)
	}
	built.Behavior.ToolNotFoundBehavior = "error"
	if got := runOptionsFor(built, nil, nil, nil, "", nil, agents.ContextBudget{}); got.Exec.ToolNotFoundBehavior != agents.ToolNotFoundError {
		t.Fatalf("explicit error = %v, want the abort", got.Exec.ToolNotFoundBehavior)
	}
}

// The resolved flag reaches the run options — this is what actually gates
// what generation spans record. Always explicit: the server resolves the
// setting's default itself, never the SDK's environment variable.
func TestRunOptionsForCarriesSensitiveFlag(t *testing.T) {
	built := &BuildResult{TraceIncludeSensitive: false}
	opts := runOptionsFor(built, nil, nil, nil, "", nil, agents.ContextBudget{})
	if opts.Observe.IncludeSensitiveData == nil || *opts.Observe.IncludeSensitiveData {
		t.Fatal("TraceIncludeSensitive=false must reach Observe.IncludeSensitiveData")
	}
	built.TraceIncludeSensitive = true
	opts = runOptionsFor(built, nil, nil, nil, "", nil, agents.ContextBudget{})
	if opts.Observe.IncludeSensitiveData == nil || !*opts.Observe.IncludeSensitiveData {
		t.Fatal("TraceIncludeSensitive=true must reach Observe.IncludeSensitiveData")
	}
}

// The SDK's own log config comes off the build, and its sensitive-data switch
// is the settings one — separate from the trace switch, because the two go to
// different places.
func TestRunOptionsForCarriesTheSDKLogger(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	got := runOptionsFor(&BuildResult{}, nil, nil, nil, "", log, agents.ContextBudget{})
	if got.Log.Logger != log {
		t.Fatal("the run's logger must reach agents.LogConfig")
	}
	if got.Log.SensitiveData {
		t.Fatal("log_sensitive_data is off unless the build says otherwise")
	}
	on := runOptionsFor(&BuildResult{LogSensitive: true}, nil, nil, nil, "", log, agents.ContextBudget{})
	if !on.Log.SensitiveData {
		t.Fatal("LogSensitive must reach the SDK")
	}
	// No logger stays no logger: runOptionsFor must not invent one. (In the
	// server this case does not arise — logging.Ctx yields a discarding
	// logger, whose Enabled is false at every level — but nothing here should
	// depend on that.)
	if off := runOptionsFor(&BuildResult{}, nil, nil, nil, "", nil, agents.ContextBudget{}); off.Log.Logger != nil {
		t.Fatal("no logger must stay no logger")
	}
}

// The notice is on exactly when the config declares a window, and the figure
// it starts from is the session's, read once at run start (invariant 28).
func TestRunOptionsForCarriesTheContextBudget(t *testing.T) {
	if got := runOptionsFor(&BuildResult{}, nil, nil, nil, "", nil, agents.ContextBudget{}); got.Model.InputFilter != nil {
		t.Fatal("no window must leave Model.InputFilter nil")
	}
	got := runOptionsFor(&BuildResult{}, nil, nil, nil, "", nil, agents.ContextBudget{Window: 1000, Occupied: 10})
	if got.Model.InputFilter == nil {
		t.Fatal("a declared window must install the budget notice")
	}
	// No window: the store is not consulted at all.
	if b := contextBudget(context.Background(), &BuildResult{}, nil, session.Ref{}); b != (agents.ContextBudget{}) {
		t.Fatalf("no window must yield the zero budget, got %+v", b)
	}
}

// The figure the run starts from is the conversation's last measured call,
// input and output together, read off the session's lifted columns.
func TestContextBudgetReadsTheLastMeasuredCall(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	ref := session.Direct(store.NewID())
	sa := store.NewEntryStoreFor(db, ref)
	sa.SetRunID(store.NewID())
	calls := []struct{ in, out int64 }{{1000, 100}, {2500, 200}}
	for _, c := range calls {
		e := session.Entry{
			Kind:   session.EntryKindItem,
			Source: agents.Source{Type: agents.SourceModel},
			Item:   json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"status":"completed"}`),
			Usage:  &session.RequestUsage{InputTokens: c.in, OutputTokens: c.out, TotalTokens: c.in + c.out},
		}
		if err := sa.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got := contextBudget(ctx, &BuildResult{ContextWindow: 8000}, sa, ref)
	if want := (agents.ContextBudget{Window: 8000, Occupied: 2700}); got != want {
		t.Fatalf("contextBudget = %+v, want %+v", got, want)
	}
}
