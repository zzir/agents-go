package bridge

import (
	"context"
	"strconv"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// runOptionsFor is the single constructor both the fresh-run and resume paths
// use — that sharing IS the regression guard (the resume path once dropped
// HandoffInputFilter and ToolNotFoundBehavior by re-listing fields by hand).
// This pins that every configured policy actually lands in the options.
func TestRunOptionsForCarriesEveryPolicy(t *testing.T) {
	built := &BuildResult{
		MaxTurns:             7,
		MaxToolConcurrency:   3,
		HandoffInputFilter:   "nest_history",
		ToolNotFoundBehavior: "continue",
		StopAtTools:          []string{"final_answer"},
		HistoryLimit:         25,
		RunGuardrails:        []agents.Guardrail{{Name: "g1"}},
	}
	opts := runOptionsFor(built, nil, nil, nil, "trust-ctx")

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
	built.HandoffInputFilter = ""
	if got := runOptionsFor(built, nil, nil, nil, ""); got.Exec.HandoffInputFilter != nil {
		t.Fatal("empty HandoffInputFilter must leave the SDK default")
	}
}

// The tri-state read of trace_include_sensitive_data: unset and garbage defer
// to the SDK default (nil), an explicit value comes through as a pointer.
func TestSensitiveTraceSettingTriState(t *testing.T) {
	if got := sensitiveTraceSetting(context.Background(), nil); got != nil {
		t.Fatalf("no settings store must mean nil, got %v", *got)
	}
	// The parse itself, without a store round-trip.
	for raw, want := range map[string]*bool{
		"false": boolPtr(false), "0": boolPtr(false), "true": boolPtr(true),
	} {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			t.Fatalf("ParseBool(%q): %v", raw, err)
		}
		if v != *want {
			t.Fatalf("ParseBool(%q) = %v, want %v", raw, v, *want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// The resolved flag reaches the run options — this is what actually gates
// what generation spans record.
func TestRunOptionsForCarriesSensitiveFlag(t *testing.T) {
	off := false
	built := &BuildResult{TraceIncludeSensitive: &off}
	opts := runOptionsFor(built, nil, nil, nil, "")
	if opts.Observe.IncludeSensitiveData == nil || *opts.Observe.IncludeSensitiveData {
		t.Fatal("TraceIncludeSensitive=false must reach Observe.IncludeSensitiveData")
	}
	if got := runOptionsFor(&BuildResult{}, nil, nil, nil, ""); got.Observe.IncludeSensitiveData != nil {
		t.Fatal("unset flag must stay nil (SDK default)")
	}
}
